package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"auto-itemizer/internal/config"
	"auto-itemizer/internal/db"
	"auto-itemizer/internal/httpapi/models"
)

var ctx = context.Background()

// fixtures is the repo's fixture folder, seen from cmd/server.
var fixtures = filepath.Join("..", "..", "fixtures")

// testConfig is the default config, with a temporary storage folder and
// database and the repo's fixtures.
func testConfig(t *testing.T) config.Config {
	t.Helper()
	storage := filepath.Join(t.TempDir(), "storage")
	return config.Config{
		Port:            "0",
		StorageDir:      storage,
		DBPath:          filepath.Join(storage, "test.db"),
		FixturesDir:     fixtures,
		MockOCR:         true,
		MockOCRFallback: filepath.Join(fixtures, "task-a", "receipt-clean.txt"),
	}
}

// openDB opens cfg's database and closes it when the test ends.
func openDB(t *testing.T, cfg config.Config) *db.DB {
	t.Helper()
	d, err := db.Open(ctx, cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// newTestHandler wires the API over cfg the way run does.
func newTestHandler(t *testing.T, cfg config.Config) http.Handler {
	t.Helper()
	provider, err := ocrProvider(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h, err := newHandler(ctx, cfg, openDB(t, cfg), provider)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// post sends a POST with no body and returns the status and body.
func post(h http.Handler, path string) (int, string) {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
	return rec.Code, rec.Body.String()
}

// upload sends content as a text/plain file named name and returns the new
// receipt's id.
func upload(t *testing.T, h http.Handler, name string, content []byte) string {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, name))
	header.Set("Content-Type", "text/plain")
	part, err := mw.CreatePart(header)
	if err != nil {
		t.Fatal(err)
	}
	part.Write(content)
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/receipts", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var created models.UploadResponse
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &created) != nil {
		t.Fatalf("upload %s = %d %s", name, rec.Code, rec.Body.String())
	}
	return created.ReceiptID
}

// TestWiring runs a receipt through the handler that newHandler builds.
func TestWiring(t *testing.T) {
	h := newTestHandler(t, testConfig(t))
	content, err := os.ReadFile(filepath.Join(fixtures, "task-a", "receipt-clean.txt"))
	if err != nil {
		t.Fatal(err)
	}

	status, body := post(h, "/receipts/"+upload(t, h, "receipt-clean.txt", content)+"/process")
	var e models.ExpenseResponse
	if status != http.StatusOK || json.Unmarshal([]byte(body), &e) != nil {
		t.Fatalf("process = %d %s", status, body)
	}
	// A VAT row means the tax names from tax_master reached the parser.
	if len(e.Taxes) != 1 || e.Taxes[0].Name != "VAT" || e.ItemizeStatus != "COMPLETE" {
		t.Errorf("process = %s; want COMPLETE with one VAT tax", body)
	}
	// Re-itemize works only if ExpenseService was connected to ReceiptService.
	if status, body := post(h, "/transactions/"+e.ID+"/itemize"); status != http.StatusOK {
		t.Errorf("itemize = %d %s, want 200", status, body)
	}
}

// With MOCK_OCR=false no fixtures are read, and process answers 501.
func TestLiveOCRNeedsNoFixtures(t *testing.T) {
	cfg := testConfig(t)
	cfg.MockOCR = false
	cfg.FixturesDir = filepath.Join(t.TempDir(), "no-such-folder")
	cfg.MockOCRFallback = filepath.Join(cfg.FixturesDir, "task-a", "receipt-clean.txt")
	h := newTestHandler(t, cfg)

	status, body := post(h, "/receipts/"+upload(t, h, "receipt.txt", []byte("TOTAL 1.00"))+"/process")
	if status != http.StatusNotImplemented || !strings.Contains(body, "LIVE_OCR_NOT_CONFIGURED") {
		t.Errorf("process = %d %s, want 501 LIVE_OCR_NOT_CONFIGURED", status, body)
	}
}

// A wrong FIXTURES_DIR stops run before it creates any folder or database.
func TestRunChecksFixturesFirst(t *testing.T) {
	cfg := testConfig(t)
	cfg.FixturesDir = filepath.Join(t.TempDir(), "no-such-folder")
	cfg.MockOCRFallback = filepath.Join(cfg.FixturesDir, "task-a", "receipt-clean.txt")

	runCtx, cancel := context.WithTimeout(ctx, 10*time.Second) // never serve for long
	defer cancel()
	if err := run(runCtx, cfg); err == nil || !strings.Contains(err.Error(), "FIXTURES_DIR") {
		t.Errorf("run = %v, want an error that mentions FIXTURES_DIR", err)
	}
	if _, err := os.Stat(cfg.StorageDir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("run created %s before checking the fixtures (stat: %v)", cfg.StorageDir, err)
	}
}

func TestNoTaxNamesRefusesToStart(t *testing.T) {
	cfg := testConfig(t)
	provider, err := ocrProvider(cfg)
	if err != nil {
		t.Fatal(err)
	}
	d := openDB(t, cfg)
	if _, err := d.ExecContext(ctx, `DELETE FROM tax_master`); err != nil {
		t.Fatal(err)
	}
	if _, err := newHandler(ctx, cfg, d, provider); err == nil || !strings.Contains(err.Error(), "tax_master") {
		t.Errorf("newHandler = %v, want it to refuse an empty tax_master", err)
	}
}

// notifyListener lets a test see when serve's shutdown closes the listener.
type notifyListener struct {
	net.Listener
	closed chan struct{}
	once   sync.Once
}

func (l *notifyListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return l.Listener.Close()
}

// startServe runs serve with h on a new local listener. Cancelling the
// returned context stands in for Ctrl-C; serve's result arrives on done.
func startServe(t *testing.T, h http.Handler) (ln *notifyListener, stop context.CancelFunc, done <-chan error) {
	t.Helper()
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln = &notifyListener{Listener: inner, closed: make(chan struct{})}
	serveCtx, stop := context.WithCancel(ctx)
	t.Cleanup(stop)
	result := make(chan error, 1)
	go func() { result <- serve(serveCtx, ln, h) }()
	return ln, stop, result
}

// waitFor returns serve's result, or fails the test if serve doesn't return.
func waitFor(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return")
		return nil
	}
}

func TestServeAnswersAndStops(t *testing.T) {
	ln, stop, done := startServe(t, newTestHandler(t, testConfig(t)))
	resp, err := http.Get("http://" + ln.Addr().String() + "/health")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /health = %d, want 200", resp.StatusCode)
	}

	stop()
	if err := waitFor(t, done); err != nil {
		t.Errorf("serve = %v, want nil after a clean stop", err)
	}
}

// A request that is running when Ctrl-C arrives still gets its answer.
func TestServeFinishesRequestsInFlight(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusOK)
	})
	ln, stop, done := startServe(t, slow)

	got := make(chan int, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/")
		if err != nil {
			got <- 0
			return
		}
		resp.Body.Close()
		got <- resp.StatusCode
	}()

	<-started      // the request is running
	stop()         // Ctrl-C
	<-ln.closed    // the shutdown has begun and is waiting for the request
	close(release) // let it finish
	if code := <-got; code != http.StatusOK {
		t.Errorf("the request in flight got %d, want 200", code)
	}
	if err := waitFor(t, done); err != nil {
		t.Errorf("serve = %v, want nil after draining", err)
	}
}

// A 12 MB upload over a real socket still gets its 413. The request log wraps
// the response writer, which hides a hook http.MaxBytesReader uses, so this
// checks the answer still arrives over a real connection (httptest's
// recorder has none).
func TestServeRefusesHugeUpload(t *testing.T) {
	ln, _, _ := startServe(t, newTestHandler(t, testConfig(t)))

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="file"; filename="huge.txt"`)
	header.Set("Content-Type", "text/plain")
	part, err := mw.CreatePart(header)
	if err != nil {
		t.Fatal(err)
	}
	part.Write(bytes.Repeat([]byte("a"), 12<<20))
	mw.Close()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// Write while reading: the server answers before it has read the whole
	// body, then stops reading, so the write may fail. That's expected.
	go func() {
		fmt.Fprintf(conn, "POST /receipts HTTP/1.1\r\nHost: localhost\r\nContent-Type: %s\r\nContent-Length: %d\r\n\r\n",
			mw.FormDataContentType(), body.Len())
		conn.Write(body.Bytes())
	}()
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("reading the answer: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", resp.StatusCode)
	}
}

// If Serve fails, serve returns its error at once instead of waiting for a
// signal, so the program exits 1.
func TestServeReturnsWhenServeFails(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	inner.Close() // Serve fails at once on a closed listener
	serveCtx, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- serve(serveCtx, inner, http.NotFoundHandler()) }()
	if err := waitFor(t, done); err == nil {
		t.Error("serve = nil, want Serve's error")
	}
}

// TestSignalChild is the server process of the signal tests below, which run
// this test binary again with SIGNAL_TEST_CHILD set. In a normal run it is
// skipped. A request to it waits until its client hangs up, so it holds the
// shutdown open.
func TestSignalChild(t *testing.T) {
	if os.Getenv("SIGNAL_TEST_CHILD") != "1" {
		t.Skip("runs only as the child process of the signal tests")
	}
	sigCtx, stop := signalContext()
	defer stop()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hold := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Println("REQUEST STARTED")
		<-r.Context().Done()
	})
	fmt.Println("ADDRESS", ln.Addr())
	if err := serve(sigCtx, ln, hold); err != nil {
		t.Fatalf("serve: %v", err)
	}
}

// startSignalChild runs TestSignalChild in a new process. It returns the
// process, its output lines (stdout and stderr) and the address it serves on.
func startSignalChild(t *testing.T) (*exec.Cmd, <-chan string, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("sends SIGINT, which Windows doesn't have")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestSignalChild$", "-test.v")
	cmd.Env = append(os.Environ(), "SIGNAL_TEST_CHILD=1")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = cmd.Stdout // one stream, so slog's lines arrive too
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill() })
	lines := make(chan string, 100)
	go func() {
		scanner := bufio.NewScanner(out)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		close(lines)
	}()
	addr := strings.TrimPrefix(waitLine(t, lines, "ADDRESS "), "ADDRESS ")
	return cmd, lines, addr
}

// waitLine returns the child's next output line that contains part.
func waitLine(t *testing.T, lines <-chan string, part string) string {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatalf("the child ended before printing %q", part)
			}
			if strings.Contains(line, part) {
				return line
			}
		case <-deadline:
			t.Fatalf("the child didn't print %q", part)
		}
	}
}

// waitExit returns the child's exit error on a channel once it has ended.
func waitExit(cmd *exec.Cmd) <-chan error {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	return done
}

// A second Ctrl-C ends the program at once, even while the shutdown waits
// for a request in flight, which would otherwise take shutdownTimeout.
func TestSecondSignalQuitsAtOnce(t *testing.T) {
	cmd, lines, addr := startSignalChild(t)
	go http.Get("http://" + addr + "/") // held open until the child ends
	waitLine(t, lines, "REQUEST STARTED")
	cmd.Process.Signal(os.Interrupt)
	waitLine(t, lines, "shutting down") // the first Ctrl-C started the shutdown
	done := waitExit(cmd)

	// Press Ctrl-C again. It is repeated because the signals are released
	// just after "shutting down" is logged, not before it.
	again := time.NewTicker(50 * time.Millisecond)
	defer again.Stop()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-done:
			return
		case <-again.C:
			cmd.Process.Signal(os.Interrupt)
		case <-deadline:
			t.Fatal("the program was still running 2 s after the second Ctrl-C")
		}
	}
}

// One Ctrl-C with nothing in flight still shuts down cleanly, with exit 0.
func TestOneSignalShutsDownCleanly(t *testing.T) {
	cmd, lines, _ := startSignalChild(t)
	cmd.Process.Signal(os.Interrupt)
	waitLine(t, lines, "shutting down")
	select {
	case err := <-waitExit(cmd):
		if err != nil {
			t.Errorf("the program ended with %v, want exit 0", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the program did not stop after Ctrl-C")
	}
}
