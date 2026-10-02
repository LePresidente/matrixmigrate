package matrix

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/aligundogdu/matrixmigrate/internal/mattermost"
)

// attachmentServer answers message sends with $e1, $e2, ... and media uploads with an mxc URI,
// remembering every send body and every uploaded filename. Uploads of a name in failUploads
// are refused.
type attachmentServer struct {
	mu          sync.Mutex
	sends       []map[string]interface{}
	sendIDs     []string
	uploads     []string
	failUploads map[string]bool
	onSend      func(sends int)
	onUpload    func(name string)
}

func (as *attachmentServer) snapshot() (sends []map[string]interface{}, ids []string, uploads []string) {
	as.mu.Lock()
	defer as.mu.Unlock()
	return append([]map[string]interface{}(nil), as.sends...), append([]string(nil), as.sendIDs...), append([]string(nil), as.uploads...)
}

func newAttachmentServer(t *testing.T, as *attachmentServer) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/send/m.room.message/"):
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			as.mu.Lock()
			as.sends = append(as.sends, body)
			id := fmt.Sprintf("$e%d", len(as.sends))
			as.sendIDs = append(as.sendIDs, id)
			n := len(as.sends)
			as.mu.Unlock()
			if as.onSend != nil {
				as.onSend(n)
			}
			_, _ = fmt.Fprintf(w, `{"event_id":%q}`, id)
		case strings.Contains(r.URL.Path, "/media/v3/upload"):
			name := r.URL.Query().Get("filename")
			if as.onUpload != nil {
				as.onUpload(name)
			}
			as.mu.Lock()
			fail := as.failUploads[name]
			if !fail {
				as.uploads = append(as.uploads, name)
			}
			as.mu.Unlock()
			if fail {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"errcode":"M_UNKNOWN","error":"refused"}`))
				return
			}
			_, _ = fmt.Fprintf(w, `{"content_uri":"mxc://example.com/%s"}`, name)
		case strings.HasSuffix(r.URL.Path, "/members"):
			_ = json.NewEncoder(w).Encode(map[string][]string{"members": {"@alice:example.com", "@bob_dev:example.com"}})
		case strings.Contains(r.URL.Path, "/account/whoami"):
			_ = json.NewEncoder(w).Encode(map[string]string{"user_id": "@admin:example.com"})
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// attachmentFixture writes each file's bytes under a temporary data directory and returns an
// importer talking to as, plus an upload-mode file config reading from that directory.
func attachmentFixture(t *testing.T, as *attachmentServer, files []mattermost.FileInfo) (*Importer, *FileConfig) {
	t.Helper()
	dataDir := t.TempDir()
	for _, f := range files {
		p := filepath.Join(dataDir, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("data-"+f.ID), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	srv := newAttachmentServer(t, as)
	c := NewClientWithRateLimit(srv.URL, "admin-token", "example.com", RateLimitConfig{})
	c.SetASToken("as-token")
	return NewImporter(c), &FileConfig{Mode: "upload", LocalDataPath: dataDir, MaxUploadSize: 1024}
}

func attachment(id, postID, name, mime string) mattermost.FileInfo {
	return mattermost.FileInfo{ID: id, PostID: postID, Name: name, Path: "files/" + id + "/" + name, Size: 7, MimeType: mime}
}

func filesByPostOf(files ...mattermost.FileInfo) map[string][]mattermost.FileInfo {
	out := make(map[string][]mattermost.FileInfo)
	for _, f := range files {
		out[f.PostID] = append(out[f.PostID], f)
	}
	return out
}

var attachmentRooms = map[string]string{"c1": "!r:example.com"}

func isBlankText(body map[string]interface{}) bool {
	return body["msgtype"] == "m.text" && strings.TrimSpace(fmt.Sprint(body["body"])) == ""
}

func TestFileOnlyPostIsMappedToItsAttachmentEvent(t *testing.T) {
	f1 := attachment("f1", "p1", "photo.png", "image/png")
	as := &attachmentServer{}
	i, fc := attachmentFixture(t, as, []mattermost.FileInfo{f1})

	posts := []mattermost.Post{{ID: "p1", ChannelID: "c1", UserID: "u-alice", Message: "  ", CreateAt: 1000}}
	result, err := i.ImportMessagesWithFiles(posts, attachmentRooms, interruptTestUsers, nil,
		filesByPostOf(f1), nil, fc, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	sends, ids, _ := as.snapshot()
	for _, s := range sends {
		if isBlankText(s) {
			t.Errorf("a blank text event was sent: %v", s)
		}
	}
	if len(sends) != 1 || sends[0]["msgtype"] != "m.image" {
		t.Fatalf("sends = %v, want exactly the image event", sends)
	}
	if result.Mapping["p1"] != ids[0] {
		t.Errorf("post p1 maps to %q, want the attachment event %q", result.Mapping["p1"], ids[0])
	}
	if result.FileMapping["f1"] != ids[0] {
		t.Errorf("file f1 maps to %q, want %q", result.FileMapping["f1"], ids[0])
	}
	if result.Stats.MessagesImported != 1 || result.Stats.FilesUploaded != 1 {
		t.Errorf("stats = %+v, want 1 message imported and 1 file uploaded", result.Stats)
	}
}

func TestReplyToFileOnlyPostThreadsOntoAttachmentEvent(t *testing.T) {
	f1 := attachment("f1", "p1", "photo.png", "image/png")
	f2 := attachment("f2", "p3", "notes.txt", "text/plain")
	as := &attachmentServer{}
	i, fc := attachmentFixture(t, as, []mattermost.FileInfo{f1, f2})

	posts := []mattermost.Post{
		{ID: "p1", ChannelID: "c1", UserID: "u-alice", Message: "", CreateAt: 1000},
		{ID: "p2", ChannelID: "c1", UserID: "u-bob", Message: "nice", RootID: "p1", CreateAt: 1001},
		{ID: "p3", ChannelID: "c1", UserID: "u-alice", Message: "", RootID: "p1", CreateAt: 1002},
		{ID: "p4", ChannelID: "c1", UserID: "u-bob", Message: "thanks", RootID: "p1", CreateAt: 1003},
	}
	result, err := i.ImportMessagesWithFiles(posts, attachmentRooms, interruptTestUsers, nil,
		filesByPostOf(f1, f2), nil, fc, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	sends, ids, _ := as.snapshot()
	if len(sends) != 4 {
		t.Fatalf("got %d sends, want 4: %v", len(sends), sends)
	}
	relation := func(n int) (root, latest string) {
		rel, _ := sends[n]["m.relates_to"].(map[string]interface{})
		reply, _ := rel["m.in_reply_to"].(map[string]interface{})
		return fmt.Sprint(rel["event_id"]), fmt.Sprint(reply["event_id"])
	}
	if root, _ := relation(1); root != result.Mapping["p1"] || root != ids[0] {
		t.Errorf("reply p2 threads onto %q, want the attachment event %q", root, ids[0])
	}
	// p3 is a file-only reply: its attachment is the reply, in the thread, after p2.
	if sends[2]["msgtype"] != "m.file" {
		t.Errorf("p3 should be sent as its attachment, got %v", sends[2])
	}
	if root, latest := relation(2); root != ids[0] || latest != ids[1] {
		t.Errorf("p3 relation = (%q, %q), want (%q, %q)", root, latest, ids[0], ids[1])
	}
	// threadLatest advanced to p3's attachment event.
	if _, latest := relation(3); latest != ids[2] || result.Mapping["p3"] != ids[2] {
		t.Errorf("p4 falls back to %q and p3 maps to %q, want both %q", latest, result.Mapping["p3"], ids[2])
	}
	if result.Stats.MessagesImported != 4 || result.Stats.RepliesImported != 3 {
		t.Errorf("stats = %+v, want 4 messages and 3 replies", result.Stats)
	}
}

func TestAlreadyImportedPostSendsOnlyUnrecordedAttachments(t *testing.T) {
	f1 := attachment("f1", "p1", "one.txt", "text/plain")
	f2 := attachment("f2", "p1", "two.txt", "text/plain")
	as := &attachmentServer{}
	i, fc := attachmentFixture(t, as, []mattermost.FileInfo{f1, f2})

	posts := []mattermost.Post{{ID: "p1", ChannelID: "c1", UserID: "u-alice", Message: "see attached", CreateAt: 1000}}
	result, err := i.ImportMessagesWithFiles(posts, attachmentRooms, interruptTestUsers,
		map[string]string{"p1": "$old"}, filesByPostOf(f1, f2), map[string]string{"f1": "$oldfile"}, fc, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	sends, ids, uploads := as.snapshot()
	if len(uploads) != 1 || uploads[0] != "two.txt" {
		t.Errorf("uploads = %v, want only two.txt", uploads)
	}
	if len(sends) != 1 || sends[0]["body"] != "two.txt" {
		t.Fatalf("sends = %v, want only the two.txt event", sends)
	}
	if ts := fmt.Sprint(sends[0]["msgtype"]); ts != "m.file" {
		t.Errorf("msgtype = %s, want m.file", ts)
	}
	if result.FileMapping["f2"] != ids[0] || result.FileMapping["f1"] != "$oldfile" {
		t.Errorf("file mapping = %v", result.FileMapping)
	}
	if result.Mapping["p1"] != "$old" || result.Stats.MessagesSkipped != 1 || result.Stats.MessagesImported != 0 {
		t.Errorf("mapping = %v stats = %+v, want p1 left as $old and counted skipped", result.Mapping, result.Stats)
	}
}

func TestAlreadyImportedReplyResendsAttachmentInItsThread(t *testing.T) {
	f1 := attachment("f1", "p2", "one.txt", "text/plain")
	as := &attachmentServer{}
	i, fc := attachmentFixture(t, as, []mattermost.FileInfo{f1})

	posts := []mattermost.Post{
		{ID: "p1", ChannelID: "c1", UserID: "u-alice", Message: "root", CreateAt: 1000},
		{ID: "p2", ChannelID: "c1", UserID: "u-bob", Message: "reply", RootID: "p1", CreateAt: 1001},
	}
	_, err := i.ImportMessagesWithFiles(posts, attachmentRooms, interruptTestUsers,
		map[string]string{"p1": "$root", "p2": "$reply"}, filesByPostOf(f1), map[string]string{}, fc, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	sends, _, _ := as.snapshot()
	if len(sends) != 1 {
		t.Fatalf("sends = %v, want one", sends)
	}
	rel, _ := sends[0]["m.relates_to"].(map[string]interface{})
	reply, _ := rel["m.in_reply_to"].(map[string]interface{})
	if rel["event_id"] != "$root" || reply["event_id"] != "$reply" {
		t.Errorf("relation = %v, want thread $root falling back to $reply", rel)
	}
}

func TestFailedUploadLeavesFileUnrecorded(t *testing.T) {
	f1 := attachment("f1", "p1", "bad.txt", "text/plain")
	f2 := attachment("f2", "p2", "alsobad.txt", "text/plain")
	as := &attachmentServer{failUploads: map[string]bool{"bad.txt": true, "alsobad.txt": true}}
	i, fc := attachmentFixture(t, as, []mattermost.FileInfo{f1, f2})

	posts := []mattermost.Post{
		{ID: "p1", ChannelID: "c1", UserID: "u-alice", Message: "with text", CreateAt: 1000},
		{ID: "p2", ChannelID: "c1", UserID: "u-alice", Message: "", CreateAt: 1001},
	}
	result, err := i.ImportMessagesWithFiles(posts, attachmentRooms, interruptTestUsers, nil,
		filesByPostOf(f1, f2), nil, fc, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := result.FileMapping["f1"]; ok {
		t.Errorf("f1 failed to upload but is recorded: %v", result.FileMapping)
	}
	if _, ok := result.FileMapping["f2"]; ok {
		t.Errorf("f2 failed to upload but is recorded: %v", result.FileMapping)
	}
	if _, ok := result.Mapping["p1"]; !ok {
		t.Errorf("p1's text was sent but p1 is not mapped")
	}
	// The file-only p2 stands as a placeholder naming its file, so it is mapped.
	if _, ok := result.Mapping["p2"]; !ok {
		t.Errorf("file-only p2 is not mapped to its placeholder: %v", result.Mapping)
	}
	if result.Stats.MessagesFailed != 0 || result.Stats.MessagesImported != 2 {
		t.Errorf("stats = %+v, want 2 imported, 0 failed", result.Stats)
	}
	sends, _, _ := as.snapshot()
	for _, s := range sends {
		if isBlankText(s) {
			t.Errorf("a blank text event was sent: %v", s)
		}
	}
}

// A file-only post none of whose attachments can be sent stands as a text event naming the
// files, so replies, reactions and pins still have an event to point at. The files stay
// unrecorded for the next run.
func TestFileOnlyPostWithAllAttachmentsFailingSendsPlaceholder(t *testing.T) {
	f1 := attachment("f1", "p1", "a.png", "image/png")
	f2 := attachment("f2", "p1", "b.txt", "text/plain")
	as := &attachmentServer{failUploads: map[string]bool{"a.png": true, "b.txt": true}}
	i, fc := attachmentFixture(t, as, []mattermost.FileInfo{f1, f2})

	posts := []mattermost.Post{
		{ID: "p1", ChannelID: "c1", UserID: "u-alice", Message: "", CreateAt: 1000},
		{ID: "p2", ChannelID: "c1", UserID: "u-bob", Message: "nice", RootID: "p1", CreateAt: 1001},
	}
	result, err := i.ImportMessagesWithFiles(posts, attachmentRooms, interruptTestUsers, nil,
		filesByPostOf(f1, f2), nil, fc, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	sends, ids, _ := as.snapshot()
	if len(sends) != 2 {
		t.Fatalf("sends = %v, want the placeholder and the reply", sends)
	}
	if sends[0]["msgtype"] != "m.text" || sends[0]["body"] != "📎 a.png, b.txt" {
		t.Errorf("first send = %v, want a text event naming both files", sends[0])
	}
	if result.Mapping["p1"] != ids[0] {
		t.Errorf("p1 maps to %q, want the placeholder %q", result.Mapping["p1"], ids[0])
	}
	if len(result.FileMapping) != 0 {
		t.Errorf("file mapping = %v, want no file recorded", result.FileMapping)
	}
	rel, _ := sends[1]["m.relates_to"].(map[string]interface{})
	if rel["rel_type"] != "m.thread" || rel["event_id"] != ids[0] {
		t.Errorf("reply p2 relation = %v, want a thread on the placeholder %q", rel, ids[0])
	}
	if result.Stats.MessagesImported != 2 || result.Stats.MessagesFailed != 0 || result.Stats.RepliesImported != 1 {
		t.Errorf("stats = %+v, want 2 imported, 0 failed, 1 reply", result.Stats)
	}
}

// The run after a placeholder sends the attachment that failed, once, and no second placeholder.
func TestPlaceholderPostGetsItsAttachmentOnTheNextRun(t *testing.T) {
	f1 := attachment("f1", "p1", "a.png", "image/png")
	as := &attachmentServer{}
	i, fc := attachmentFixture(t, as, []mattermost.FileInfo{f1})
	onDisk := filepath.Join(fc.LocalDataPath, filepath.FromSlash(f1.Path))
	saved, err := os.ReadFile(onDisk)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(onDisk); err != nil {
		t.Fatal(err)
	}

	posts := []mattermost.Post{{ID: "p1", ChannelID: "c1", UserID: "u-alice", Message: "", CreateAt: 1000}}
	first, err := i.ImportMessagesWithFiles(posts, attachmentRooms, interruptTestUsers, nil,
		filesByPostOf(f1), nil, fc, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := first.Mapping["p1"]; !ok {
		t.Fatalf("first run did not map p1: %v", first.Mapping)
	}

	if err := os.WriteFile(onDisk, saved, 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := i.ImportMessagesWithFiles(posts, attachmentRooms, interruptTestUsers, first.Mapping,
		filesByPostOf(f1), first.FileMapping, fc, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	sends, ids, uploads := as.snapshot()
	if len(sends) != 2 || sends[0]["msgtype"] != "m.text" || sends[1]["msgtype"] != "m.image" {
		t.Fatalf("sends = %v, want the placeholder then the image, nothing else", sends)
	}
	if len(uploads) != 1 {
		t.Errorf("uploads = %v, want exactly one", uploads)
	}
	if second.FileMapping["f1"] != ids[1] || second.Mapping["p1"] != ids[0] {
		t.Errorf("mapping = %v files = %v, want p1 on the placeholder and f1 on the image", second.Mapping, second.FileMapping)
	}
}

// An interrupt that stops a file-only post before any attachment is sent leaves it unsent and
// unmapped: no placeholder goes out for a post the next run will send properly.
func TestInterruptBeforeFirstAttachmentOfFileOnlyPostSendsNothing(t *testing.T) {
	f1 := attachment("f1", "p1", "a.png", "image/png")
	f2 := attachment("f2", "p1", "b.txt", "text/plain")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	as := &attachmentServer{
		failUploads: map[string]bool{"a.png": true},
		onUpload:    func(string) { cancel() },
	}
	i, fc := attachmentFixture(t, as, []mattermost.FileInfo{f1, f2})
	i.SetContext(ctx)

	posts := []mattermost.Post{{ID: "p1", ChannelID: "c1", UserID: "u-alice", Message: "", CreateAt: 1000}}
	result, err := i.ImportMessagesWithFiles(posts, attachmentRooms, interruptTestUsers, nil,
		filesByPostOf(f1, f2), nil, fc, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	sends, _, _ := as.snapshot()
	if len(sends) != 0 {
		t.Errorf("sends = %v, want none", sends)
	}
	if len(result.Mapping) != 0 || len(result.FileMapping) != 0 {
		t.Errorf("mapping = %v files = %v, want nothing recorded", result.Mapping, result.FileMapping)
	}
}

func TestFileOnlyPostWithoutAttachmentEventsNamesTheFiles(t *testing.T) {
	f1 := attachment("f1", "p1", "a.png", "image/png")
	f2 := attachment("f2", "p1", "b.txt", "text/plain")
	for _, fc := range []*FileConfig{{Mode: "skip"}, {Mode: "link"}} {
		t.Run(fc.Mode, func(t *testing.T) {
			as := &attachmentServer{}
			i, _ := attachmentFixture(t, as, nil)
			posts := []mattermost.Post{{ID: "p1", ChannelID: "c1", UserID: "u-alice", Message: "", CreateAt: 1000}}
			result, err := i.ImportMessagesWithFiles(posts, attachmentRooms, interruptTestUsers, nil,
				filesByPostOf(f1, f2), nil, fc, nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			sends, ids, _ := as.snapshot()
			if len(sends) != 1 || sends[0]["body"] != "📎 a.png, b.txt" {
				t.Fatalf("sends = %v, want one text event naming the attachments", sends)
			}
			if result.Mapping["p1"] != ids[0] {
				t.Errorf("mapping = %v", result.Mapping)
			}
		})
	}
}

// An interrupt after a post's text is sent but before its attachments leaves them unrecorded,
// so the next run - which sees the post as imported - still sends them.
func TestInterruptBeforeAttachmentsLeavesThemUnrecorded(t *testing.T) {
	f1 := attachment("f1", "p1", "one.txt", "text/plain")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	as := &attachmentServer{onSend: func(int) { cancel() }}
	i, fc := attachmentFixture(t, as, []mattermost.FileInfo{f1})
	i.SetContext(ctx)

	posts := []mattermost.Post{{ID: "p1", ChannelID: "c1", UserID: "u-alice", Message: "see attached", CreateAt: 1000}}
	result, err := i.ImportMessagesWithFiles(posts, attachmentRooms, interruptTestUsers, nil,
		filesByPostOf(f1), nil, fc, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, uploads := as.snapshot()
	if _, ok := result.Mapping["p1"]; !ok || len(uploads) != 0 {
		t.Fatalf("mapping = %v uploads = %v, want p1 mapped and nothing uploaded", result.Mapping, uploads)
	}
	if _, ok := result.FileMapping["f1"]; ok {
		t.Errorf("f1 was never sent but is recorded")
	}
}
