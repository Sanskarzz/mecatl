package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stacklok/mecatl/engine/session"
	"github.com/stacklok/mecatl/internal/adapter/tools"
)

// fsPeer is a scripted ACP CLIENT for fsWorkspace unit tests: it answers the
// agent's outbound fs/read_text_file / fs/write_text_file requests against an
// in-memory buffer map (the editor's "buffers"), so a test can assert that
// Read/Write delegate (and never touch disk) and count the fs/* calls issued.
// It speaks newline-delimited JSON (ndjson) over a pipe, the same as a real editor.
type fsPeer struct {
	t *testing.T

	mu      sync.Mutex
	buffers map[string]string // absolute path -> buffer content

	reads  atomic.Int64 // count of fs/read_text_file requests served
	writes atomic.Int64 // count of fs/write_text_file requests served

	// hang, when true, makes the peer NEVER respond to a request (it reads and
	// drops it), so a test can assert the per-call fs/* timeout fires.
	hang atomic.Bool
	// ambiguousRead, when true, answers a fs/read_text_file MISS with a NON-
	// not-found error (a transport-shaped fault), so a test can exercise the
	// Stat fail-safe (ambiguous read -> report EXISTS).
	ambiguousRead atomic.Bool

	toConn   *io.PipeWriter // peer -> conn.r (responses)
	fromConn *bufio.Reader  // conn.w -> peer (the agent's requests)
}

// newFSPeerConn wires a Conn whose outbound Calls are answered by an fsPeer.
// It returns the Conn (already Serve-ing so it can deliver responses), the peer
// (seeded with the given buffers), and a cancel func to stop serving.
func newFSPeerConn(t *testing.T, buffers map[string]string) (*Conn, *fsPeer, context.CancelFunc) {
	t.Helper()
	connReadR, peerWriteW := io.Pipe() // peer writes responses -> conn reads
	peerReadR, connWriteW := io.Pipe() // conn writes requests  -> peer reads
	conn := NewConn(connReadR, connWriteW, nil)
	if buffers == nil {
		buffers = map[string]string{}
	}
	peer := &fsPeer{
		t:        t,
		buffers:  buffers,
		toConn:   peerWriteW,
		fromConn: bufio.NewReader(peerReadR),
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = conn.Serve(ctx) }()
	go peer.loop()
	t.Cleanup(func() {
		cancel()
		_ = peerWriteW.Close()
		_ = connWriteW.Close()
	})
	return conn, peer, cancel
}

// get/set/has manipulate the peer's buffers out of band (simulating the editor's
// own buffer mutations) for the ledger tests.
func (p *fsPeer) get(abs string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	v, ok := p.buffers[abs]
	return v, ok
}

func (p *fsPeer) set(abs, content string) {
	p.mu.Lock()
	p.buffers[abs] = content
	p.mu.Unlock()
}

// loop reads the agent's fs/* requests and answers them from the buffer map.
func (p *fsPeer) loop() {
	for {
		raw, err := p.fromConn.ReadBytes('\n')
		line := strings.TrimRight(string(raw), "\r\n")
		if line == "" {
			if err != nil {
				return // EOF / closed pipe on a blank trailing line
			}
			continue // bare blank line between messages
		}
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if uerr := json.Unmarshal([]byte(line), &m); uerr != nil {
			return
		}
		if p.hang.Load() {
			// Read the request but never answer it: the agent's per-call timeout must
			// fire. Keep looping so a later un-hung request could still be served.
			continue
		}
		switch m.Method {
		case methodFSReadTextFile:
			p.reads.Add(1)
			var req fsReadTextFileRequest
			_ = json.Unmarshal(m.Params, &req)
			content, ok := p.get(req.Path)
			if !ok {
				if p.ambiguousRead.Load() {
					// A transport-shaped fault (NOT a clean not-found): exercises the
					// Stat fail-safe path.
					p.respondErr(m.ID, "internal editor error: connection reset")
					continue
				}
				// A clean editor "file does not exist" — the message contains a marker
				// isFSNotFound recognizes, so the agent classifies it as genuinely new.
				p.respondErr(m.ID, fmt.Sprintf("no such file or directory: %s", req.Path))
				continue
			}
			p.respond(m.ID, fsReadTextFileResponse{Content: content})
		case methodFSWriteTextFile:
			p.writes.Add(1)
			var req fsWriteTextFileRequest
			_ = json.Unmarshal(m.Params, &req)
			p.set(req.Path, req.Content)
			p.respond(m.ID, struct{}{})
		default:
			p.respondErr(m.ID, "unexpected method "+m.Method)
		}
	}
}

func (p *fsPeer) respond(id json.RawMessage, result any) {
	raw, _ := json.Marshal(result)
	p.writeFrame(map[string]any{"jsonrpc": "2.0", "id": id, "result": json.RawMessage(raw)})
}

func (p *fsPeer) respondErr(id json.RawMessage, msg string) {
	p.writeFrame(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32000, "message": msg}})
}

func (p *fsPeer) writeFrame(m any) {
	body, _ := json.Marshal(m)
	body = append(body, '\n')
	if _, err := p.toConn.Write(body); err != nil {
		// The conn may have closed (test teardown); ignore.
		return
	}
}

// newTestFSWorkspace builds an fsWorkspace over a peer-backed Conn, rooted at a
// fresh tempdir, seeding the editor buffers under their ABSOLUTE paths derived
// from rel -> root. It returns the workspace, the peer, and the resolved root.
func newTestFSWorkspace(t *testing.T, files map[string]string) (*fsWorkspace, *fsPeer, string) {
	t.Helper()
	root := t.TempDir()
	// Seed buffers keyed by the absolute path the workspace will compute (the
	// EvalSymlinks-resolved root, which is what osfs.Root() returns).
	conn, peer, _ := newFSPeerConn(t, nil)
	ws, err := newFSWorkspace(conn, "sess-test", root)
	if err != nil {
		t.Fatalf("newFSWorkspace: %v", err)
	}
	for rel, content := range files {
		peer.set(filepath.Join(ws.Root(), rel), content)
	}
	return ws, peer, ws.Root()
}

// --- T1: Read/Write delegate, not disk --------------------------------------

func TestFSWorkspaceReadWriteDelegate(t *testing.T) {
	ctx := context.Background()
	ws, peer, root := newTestFSWorkspace(t, map[string]string{"a.txt": "hello"})

	got, err := ws.Read(ctx, "a.txt")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("Read = %q, want %q", got, "hello")
	}
	if peer.reads.Load() != 1 {
		t.Fatalf("expected 1 fs/read, got %d", peer.reads.Load())
	}

	if err := ws.Write(ctx, "a.txt", []byte("world")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if peer.writes.Load() != 1 {
		t.Fatalf("expected 1 fs/write, got %d", peer.writes.Load())
	}
	// The bytes landed in the peer's buffer (delegated), NOT on disk.
	if v, _ := peer.get(filepath.Join(root, "a.txt")); v != "world" {
		t.Fatalf("peer buffer = %q, want %q", v, "world")
	}
	if _, err := os.Stat(filepath.Join(root, "a.txt")); err == nil {
		t.Fatalf("Write touched disk at %s; it must delegate to the editor", filepath.Join(root, "a.txt"))
	}
}

// --- T2: Edit's three invariants over the delegating workspace ---------------

func TestFSWorkspaceEditInvariants(t *testing.T) {
	ctx := context.Background()
	edit := tools.EditTool{}

	// (i) edit without prior Read -> rejected (invariant #1).
	ws, peer, root := newTestFSWorkspace(t, map[string]string{"f.go": "package x\nvar A = 1\nvar B = 1\n"})
	res := runEdit(t, edit, ws, "f.go", "var A = 1", "var A = 2", false)
	if !res.IsError {
		t.Fatalf("(i) edit without prior read should be rejected")
	}

	// (ii) Read, then the editor buffer changes out of band, then Edit -> rejected.
	if _, err := ws.Read(ctx, "f.go"); err != nil {
		t.Fatalf("Read: %v", err)
	}
	ws.RecordRead("f.go", "")
	peer.set(filepath.Join(root, "f.go"), "package x\nvar A = 999\n") // editor edited the buffer
	res = runEdit(t, edit, ws, "f.go", "var A = 1", "var A = 2", false)
	if !res.IsError {
		t.Fatalf("(ii) edit after buffer changed should be rejected (unchanged-since)")
	}

	// (iii) Read then Edit with a unique old_string -> succeeds; write delegated.
	ws2, peer2, root2 := newTestFSWorkspace(t, map[string]string{"f.go": "package x\nvar A = 1\nvar C = 3\n"})
	if _, err := ws2.Read(ctx, "f.go"); err != nil {
		t.Fatalf("Read: %v", err)
	}
	ws2.RecordRead("f.go", "")
	res = runEdit(t, edit, ws2, "f.go", "var A = 1", "var A = 2", false)
	if res.IsError {
		t.Fatalf("(iii) unique edit should succeed, got error: %s", res.Content)
	}
	if v, _ := peer2.get(filepath.Join(root2, "f.go")); !strings.Contains(v, "var A = 2") {
		t.Fatalf("(iii) edit did not land in the peer buffer: %q", v)
	}

	// (iv) non-unique old_string without replace_all -> rejected (invariant #3).
	ws3, _, _ := newTestFSWorkspace(t, map[string]string{"f.go": "dup\ndup\n"})
	if _, err := ws3.Read(ctx, "f.go"); err != nil {
		t.Fatalf("Read: %v", err)
	}
	ws3.RecordRead("f.go", "")
	res = runEdit(t, edit, ws3, "f.go", "dup", "x", false)
	if !res.IsError {
		t.Fatalf("(iv) non-unique edit without replace_all should be rejected")
	}

	// (v) old_string not found -> rejected (invariant #2, exact match).
	ws4, _, _ := newTestFSWorkspace(t, map[string]string{"f.go": "alpha\n"})
	if _, err := ws4.Read(ctx, "f.go"); err != nil {
		t.Fatalf("Read: %v", err)
	}
	ws4.RecordRead("f.go", "")
	res = runEdit(t, edit, ws4, "f.go", "missing", "x", false)
	if !res.IsError {
		t.Fatalf("(v) edit with absent old_string should be rejected")
	}
}

// runEdit executes the real EditTool against ws and returns the result.
func runEdit(t *testing.T, edit tools.EditTool, ws *fsWorkspace, path, oldS, newS string, replaceAll bool) session.ToolResult {
	t.Helper()
	args := map[string]any{"path": path, "old_string": oldS, "new_string": newS}
	if replaceAll {
		args["replace_all"] = true
	}
	raw, _ := json.Marshal(args)
	res, err := edit.Execute(context.Background(), session.ToolCall{ID: "c1", Name: "Edit", Args: raw}, ws)
	if err != nil {
		t.Fatalf("edit returned hard error: %v", err)
	}
	return res
}

// --- T3: buffer-keyed ledger -------------------------------------------------

func TestFSWorkspaceBufferKeyedLedger(t *testing.T) {
	ctx := context.Background()
	ws, peer, root := newTestFSWorkspace(t, map[string]string{"a.txt": "v1"})

	ws.RecordRead("a.txt", "")
	ok, err := ws.WasReadUnchanged(ctx, "a.txt")
	if err != nil {
		t.Fatalf("WasReadUnchanged: %v", err)
	}
	if !ok {
		t.Fatalf("expected unchanged right after RecordRead")
	}

	// Mutate the editor buffer out of band -> the fingerprint must change.
	peer.set(filepath.Join(root, "a.txt"), "v2")
	ok, err = ws.WasReadUnchanged(ctx, "a.txt")
	if err != nil {
		t.Fatalf("WasReadUnchanged: %v", err)
	}
	if ok {
		t.Fatalf("expected changed after the peer buffer was mutated")
	}

	// A never-recorded path is "changed" (false, nil), not an error.
	ok, err = ws.WasReadUnchanged(ctx, "never.txt")
	if err != nil || ok {
		t.Fatalf("never-recorded path: ok=%v err=%v, want false/nil", ok, err)
	}
}

// --- T4: Grep/Glob hybrid -> disk results, no fs/* calls ---------------------

func TestFSWorkspaceGrepGlobHybrid(t *testing.T) {
	ctx := context.Background()
	ws, peer, root := newTestFSWorkspace(t, nil)

	// Write a file to DISK directly (the composed osfs view searches disk).
	if err := os.WriteFile(filepath.Join(root, "code.go"), []byte("package x\nfunc Hello() {}\n"), 0o644); err != nil {
		t.Fatalf("seed disk file: %v", err)
	}

	matches, err := ws.Grep(ctx, "func Hello", "")
	if err != nil {
		t.Fatalf("Grep: %v", err)
	}
	if len(matches) != 1 || matches[0].Path != "code.go" {
		t.Fatalf("Grep returned %+v, want one match in code.go", matches)
	}

	globbed, err := ws.Glob(ctx, "*.go")
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	if len(globbed) != 1 || globbed[0] != "code.go" {
		t.Fatalf("Glob returned %v, want [code.go]", globbed)
	}

	// Grep/Glob must NOT issue any fs/* calls (they read disk).
	if peer.reads.Load() != 0 || peer.writes.Load() != 0 {
		t.Fatalf("Grep/Glob issued fs/* calls: reads=%d writes=%d", peer.reads.Load(), peer.writes.Load())
	}
}

// --- T5: concurrency — N concurrent Reads race-clean -------------------------

func TestFSWorkspaceConcurrentReads(t *testing.T) {
	ctx := context.Background()
	files := map[string]string{}
	for i := 0; i < 16; i++ {
		files[fmt.Sprintf("f%d.txt", i)] = fmt.Sprintf("content-%d", i)
	}
	ws, _, _ := newTestFSWorkspace(t, files)

	var wg sync.WaitGroup
	errs := make(chan error, len(files))
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rel := fmt.Sprintf("f%d.txt", i)
			want := fmt.Sprintf("content-%d", i)
			got, err := ws.Read(ctx, rel)
			if err != nil {
				errs <- fmt.Errorf("read %s: %w", rel, err)
				return
			}
			if string(got) != want {
				errs <- fmt.Errorf("read %s = %q, want %q", rel, got, want)
			}
			// Also exercise the ledger concurrently.
			ws.RecordRead(rel, "")
			if ok, lerr := ws.WasReadUnchanged(ctx, rel); lerr != nil || !ok {
				errs <- fmt.Errorf("ledger %s: ok=%v err=%v", rel, ok, lerr)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// --- path confinement --------------------------------------------------------

func TestFSWorkspacePathConfinement(t *testing.T) {
	ctx := context.Background()
	ws, peer, _ := newTestFSWorkspace(t, nil)

	for _, bad := range []string{"../escape.txt", "../../etc/passwd", "/etc/passwd", "a/../../b"} {
		if _, err := ws.Read(ctx, bad); err == nil {
			t.Errorf("Read(%q) should be rejected as an escape", bad)
		}
		if err := ws.Write(ctx, bad, []byte("x")); err == nil {
			t.Errorf("Write(%q) should be rejected as an escape", bad)
		}
	}
	// None of the escapes should have reached the editor.
	if peer.reads.Load() != 0 || peer.writes.Load() != 0 {
		t.Fatalf("an escaping path was delegated: reads=%d writes=%d", peer.reads.Load(), peer.writes.Load())
	}
}

// --- buffer-only Stat -> Write read-before-overwrite gate engages -----------

// A file that exists ONLY as an unsaved editor buffer (present in the peer,
// absent on disk) must Stat as EXISTING, so the real WriteTool's
// read-before-overwrite gate engages: a Write WITHOUT a prior Read is refused;
// after Read (+unchanged) it succeeds and lands in the buffer.
func TestFSWorkspaceBufferOnlyStatGatesWrite(t *testing.T) {
	ctx := context.Background()
	// Seed the file as a buffer ONLY (not on disk).
	ws, peer, root := newTestFSWorkspace(t, map[string]string{"buf.txt": "original"})
	abs := filepath.Join(root, "buf.txt")
	if _, err := os.Stat(abs); err == nil {
		t.Fatalf("precondition: %s must NOT exist on disk", abs)
	}

	// Stat must report EXISTS (via the buffer probe), not not-exist.
	info, err := ws.Stat(ctx, "buf.txt")
	if err != nil {
		t.Fatalf("Stat of buffer-only file: %v (want exists)", err)
	}
	if info.IsDir || info.Size != int64(len("original")) {
		t.Fatalf("Stat synthesized FileInfo = %+v, want a regular file of size %d", info, len("original"))
	}

	write := tools.WriteTool{}

	// (a) Write WITHOUT a prior Read -> refused (read-before-overwrite engages).
	res := runWrite(t, write, ws, "buf.txt", "clobbered")
	if !res.IsError {
		t.Fatalf("Write to an unsaved buffer without a prior Read must be refused; got success")
	}
	if v, _ := peer.get(abs); v != "original" {
		t.Fatalf("the buffer was clobbered despite the gate: %q", v)
	}

	// (b) Read then Write -> succeeds, lands in the buffer.
	if _, err := ws.Read(ctx, "buf.txt"); err != nil {
		t.Fatalf("Read: %v", err)
	}
	ws.RecordRead("buf.txt", "")
	res = runWrite(t, write, ws, "buf.txt", "clobbered")
	if res.IsError {
		t.Fatalf("Write after Read should succeed, got error: %s", res.Content)
	}
	if v, _ := peer.get(abs); v != "clobbered" {
		t.Fatalf("Write did not land in the buffer: %q", v)
	}
}

// A genuinely new file (absent on disk AND in the editor) Stats as not-exist, so
// Write is allowed without a prior read.
func TestFSWorkspaceGenuinelyNewStatNotExist(t *testing.T) {
	ctx := context.Background()
	ws, _, _ := newTestFSWorkspace(t, nil) // empty: peer returns a clean not-found
	if _, err := ws.Stat(ctx, "new.txt"); err == nil {
		t.Fatalf("Stat of a genuinely-new file should report not-exist")
	}
}

// An AMBIGUOUS fs/read fault (not a clean not-found) on a disk-absent file must
// fail SAFE: Stat reports EXISTS so the write-before-overwrite gate engages.
func TestFSWorkspaceAmbiguousReadStatFailsSafe(t *testing.T) {
	ctx := context.Background()
	ws, peer, _ := newTestFSWorkspace(t, nil)
	peer.ambiguousRead.Store(true)
	info, err := ws.Stat(ctx, "mystery.txt")
	if err != nil {
		t.Fatalf("ambiguous read should fail safe to EXISTS, got error: %v", err)
	}
	if info.IsDir {
		t.Fatalf("fail-safe FileInfo should be a regular file: %+v", info)
	}
}

// runWrite executes the real WriteTool against ws and returns the result.
func runWrite(t *testing.T, write tools.WriteTool, ws *fsWorkspace, path, content string) session.ToolResult {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"path": path, "content": content})
	res, err := write.Execute(context.Background(), session.ToolCall{ID: "w1", Name: "Write", Args: raw}, ws)
	if err != nil {
		t.Fatalf("write returned hard error: %v", err)
	}
	return res
}

// --- symlink escape rejected, never delegated -------------------------------

// A symlink INSIDE the workspace root that points OUTSIDE it must be rejected by
// absPath's best-effort symlink re-confinement, so Read/Write never delegate a
// path that resolves out of root.
func TestFSWorkspaceSymlinkEscapeRejected(t *testing.T) {
	ctx := context.Background()
	// outside is a sibling of the workspace root, containing a secret.
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("top secret"), 0o644); err != nil {
		t.Fatalf("seed secret: %v", err)
	}

	ws, peer, root := newTestFSWorkspace(t, nil)
	// Create an in-root symlink to the outside secret (as `ln -s` via Bash would).
	link := filepath.Join(root, "evil")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlinks unsupported on this platform: %v", err)
	}
	// Also a symlinked DIRECTORY component leaving the root.
	linkDir := filepath.Join(root, "escapedir")
	if err := os.Symlink(outside, linkDir); err != nil {
		t.Skipf("symlinks unsupported on this platform: %v", err)
	}

	for _, bad := range []string{"evil", "escapedir/secret.txt"} {
		if _, err := ws.Read(ctx, bad); err == nil {
			t.Errorf("Read(%q) through an escaping symlink should be rejected", bad)
		}
		if err := ws.Write(ctx, bad, []byte("x")); err == nil {
			t.Errorf("Write(%q) through an escaping symlink should be rejected", bad)
		}
	}
	if peer.reads.Load() != 0 || peer.writes.Load() != 0 {
		t.Fatalf("an escaping symlink was delegated: reads=%d writes=%d", peer.reads.Load(), peer.writes.Load())
	}
}

// An in-root symlink pointing to an in-root target is allowed (it does not
// escape) — confinement must not be over-broad.
func TestFSWorkspaceInRootSymlinkAllowed(t *testing.T) {
	ctx := context.Background()
	ws, peer, root := newTestFSWorkspace(t, nil)
	target := filepath.Join(root, "real.txt")
	if err := os.WriteFile(target, []byte("hi"), 0o644); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	link := filepath.Join(root, "alias.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	// Seed the buffer at the link's real resolved path so the read returns content.
	peer.set(filepath.Join(root, "alias.txt"), "hi")
	if _, err := ws.Read(ctx, "alias.txt"); err != nil {
		t.Fatalf("Read of an in-root symlink should be allowed, got: %v", err)
	}
}

// --- per-call fs/* timeout fires against a non-responsive editor ------------

func TestFSWorkspaceCallTimeout(t *testing.T) {
	ws, peer, _ := newTestFSWorkspace(t, map[string]string{"a.txt": "x"})
	// Shrink the per-call bound and make the peer never respond.
	ws.callTimeout = 100 * time.Millisecond
	peer.hang.Store(true)

	done := make(chan error, 1)
	go func() {
		_, err := ws.Read(context.Background(), "a.txt")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Read against a hung editor should time out, got nil error")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Read error = %v, want a deadline-exceeded timeout", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Read did not return within the bound; the per-call timeout did not fire")
	}

	// Write must also be bounded.
	go func() {
		done <- ws.Write(context.Background(), "a.txt", []byte("y"))
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Write error = %v, want a deadline-exceeded timeout", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Write did not return within the bound")
	}
}

// guard against a flaky timeout: ensure a Read completes promptly when the peer
// responds normally (the timeout is an upper bound, not a delay).
func TestFSWorkspaceReadTimeoutSane(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ws, _, _ := newTestFSWorkspace(t, map[string]string{"a.txt": "x"})
	if _, err := ws.Read(ctx, "a.txt"); err != nil {
		t.Fatalf("Read: %v", err)
	}
}
