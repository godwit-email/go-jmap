package jmapmemserver_test

import (
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"

	"git.sr.ht/~rockorager/go-jmap"
	"git.sr.ht/~rockorager/go-jmap/core/push"
	"git.sr.ht/~rockorager/go-jmap/jmapmemserver"
	"git.sr.ht/~rockorager/go-jmap/mail/email"
	"git.sr.ht/~rockorager/go-jmap/mail/mailbox"
)

func newClient(t *testing.T, s *jmapmemserver.Server) *jmap.Client {
	t.Helper()
	c := (&jmap.Client{SessionEndpoint: s.URL() + "/.well-known/jmap"}).WithBasicAuth("alice", "secret")
	if err := c.Authenticate(); err != nil {
		t.Fatal(err)
	}
	return c
}

func do(t *testing.T, c *jmap.Client, methods ...jmap.Method) []*jmap.Invocation {
	t.Helper()
	req := &jmap.Request{}
	for _, m := range methods {
		req.Invoke(m)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp.Responses
}

func TestSessionAndMailboxGet(t *testing.T) {
	s := jmapmemserver.New("alice", "secret")
	defer s.Close()
	inbox := s.CreateMailbox("Inbox", mailbox.RoleInbox)

	c := newClient(t, s)
	if c.Session.PrimaryAccounts["urn:ietf:params:jmap:mail"] != s.AccountID() {
		t.Fatalf("primary mail account = %q", c.Session.PrimaryAccounts["urn:ietf:params:jmap:mail"])
	}
	got := do(t, c, &mailbox.Get{Account: s.AccountID()})[0].Args.(*mailbox.GetResponse)
	if len(got.List) != 1 || got.List[0].ID != inbox || got.List[0].Role != mailbox.RoleInbox || got.List[0].Name != "Inbox" {
		t.Fatalf("Mailbox/get = %+v", got.List)
	}
}

const msg1 = "From: Bob <bob@example.com>\r\nTo: alice@example.com\r\nSubject: =?UTF-8?Q?caf=C3=A9?=\r\nMessage-ID: <one@example.com>\r\nDate: Mon, 02 Jan 2006 15:04:05 +0000\r\n\r\nhello\r\n"

func TestQueryGetAndDownload(t *testing.T) {
	s := jmapmemserver.New("alice", "secret")
	defer s.Close()
	inbox := s.CreateMailbox("Inbox", mailbox.RoleInbox)
	other := s.CreateMailbox("Other", "")
	id := s.AddEmail([]jmap.ID{inbox}, []byte(msg1), "$seen")
	s.AddEmail([]jmap.ID{other}, []byte(msg1))

	c := newClient(t, s)
	req := &jmap.Request{}
	q := req.Invoke(&email.Query{Account: s.AccountID(), Filter: &email.FilterCondition{InMailbox: inbox}, CalculateTotal: true})
	req.Invoke(&email.Get{Account: s.AccountID(), ReferenceIDs: &jmap.ResultReference{ResultOf: q, Name: "Email/query", Path: "/ids"}})
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	qr := resp.Responses[0].Args.(*email.QueryResponse)
	if len(qr.IDs) != 1 || qr.IDs[0] != id || qr.Total != 1 {
		t.Fatalf("Email/query = %+v", qr)
	}
	gr := resp.Responses[1].Args.(*email.GetResponse)
	if len(gr.List) != 1 {
		t.Fatalf("Email/get = %+v", gr)
	}
	e := gr.List[0]
	if e.ID != id || e.Subject != "café" || e.From[0].Email != "bob@example.com" ||
		len(e.MessageID) != 1 || e.MessageID[0] != "one@example.com" ||
		!e.Keywords["$seen"] || !e.MailboxIDs[inbox] || e.Size != uint64(len(msg1)) {
		t.Fatalf("email = %+v", e)
	}

	rc, err := c.Download(s.AccountID(), e.BlobID)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if b, _ := io.ReadAll(rc); string(b) != msg1 {
		t.Fatalf("blob = %q", b)
	}
}

func emailChanges(t *testing.T, c *jmap.Client, s *jmapmemserver.Server, since string, max uint64) any {
	t.Helper()
	return do(t, c, &email.Changes{Account: s.AccountID(), SinceState: since, MaxChanges: max})[0].Args
}

func TestChanges(t *testing.T) {
	s := jmapmemserver.New("alice", "secret")
	defer s.Close()
	inbox := s.CreateMailbox("Inbox", mailbox.RoleInbox)
	c := newClient(t, s)

	st := do(t, c, &email.Get{Account: s.AccountID(), IDs: []jmap.ID{}})[0].Args.(*email.GetResponse).State
	mst := do(t, c, &mailbox.Get{Account: s.AccountID()})[0].Args.(*mailbox.GetResponse).State
	a := s.AddEmail([]jmap.ID{inbox}, []byte(msg1))
	b := s.AddEmail([]jmap.ID{inbox}, []byte(msg1))

	page := emailChanges(t, c, s, st, 1).(*email.ChangesResponse)
	if len(page.Created) != 1 || page.Created[0] != a || !page.HasMoreChanges {
		t.Fatalf("first page = %+v", page)
	}
	page = emailChanges(t, c, s, page.NewState, 1).(*email.ChangesResponse)
	if len(page.Created) != 1 || page.Created[0] != b || page.HasMoreChanges {
		t.Fatalf("second page = %+v", page)
	}

	mc := do(t, c, &mailbox.Changes{Account: s.AccountID(), SinceState: mst})[0].Args.(*mailbox.ChangesResponse)
	if len(mc.Created)+len(mc.Updated)+len(mc.Destroyed) != 0 {
		t.Fatalf("Mailbox/changes after only email changes = %+v", mc)
	}
	other := s.CreateMailbox("Other", "")
	mc = do(t, c, &mailbox.Changes{Account: s.AccountID(), SinceState: mst})[0].Args.(*mailbox.ChangesResponse)
	if len(mc.Created) != 1 || mc.Created[0] != other {
		t.Fatalf("Mailbox/changes = %+v", mc)
	}

	s.ExpireState()
	if err, ok := emailChanges(t, c, s, page.NewState, 0).(*jmap.MethodError); !ok || err.Type != "cannotCalculateChanges" {
		t.Fatalf("after ExpireState: %+v", err)
	}
	fresh := do(t, c, &email.Get{Account: s.AccountID(), IDs: []jmap.ID{}})[0].Args.(*email.GetResponse).State
	if _, ok := emailChanges(t, c, s, fresh, 0).(*email.ChangesResponse); !ok {
		t.Fatal("a state fetched after ExpireState should still work")
	}
}

func TestEmailSet(t *testing.T) {
	s := jmapmemserver.New("alice", "secret")
	defer s.Close()
	inbox := s.CreateMailbox("Inbox", mailbox.RoleInbox)
	archive := s.CreateMailbox("Archive", mailbox.RoleArchive)
	a := s.AddEmail([]jmap.ID{inbox}, []byte(msg1), "$flagged")
	b := s.AddEmail([]jmap.ID{inbox}, []byte(msg1))
	c := newClient(t, s)
	st := do(t, c, &email.Get{Account: s.AccountID(), IDs: []jmap.ID{a}})[0].Args.(*email.GetResponse).State

	set := do(t, c, &email.Set{Account: s.AccountID(), IfInState: st,
		Update: map[jmap.ID]jmap.Patch{
			a:      {"keywords/$seen": true, "keywords/$flagged": nil, "mailboxIds": map[jmap.ID]bool{archive: true}},
			"nope": {"keywords/$seen": true},
			b:      {"mailboxIds": map[jmap.ID]bool{"m999": true}},
		},
		Destroy: []jmap.ID{b, "gone"},
	})[0].Args.(*email.SetResponse)
	if _, ok := set.Updated[a]; !ok || set.NotUpdated["nope"].Type != "notFound" ||
		set.NotUpdated[b].Type != "invalidProperties" || len(set.Destroyed) != 1 || set.Destroyed[0] != b ||
		set.NotDestroyed["gone"].Type != "notFound" || set.OldState != st || set.NewState == st {
		t.Fatalf("Email/set = %+v", set)
	}

	got := do(t, c, &email.Get{Account: s.AccountID(), IDs: []jmap.ID{a, b}})[0].Args.(*email.GetResponse)
	e := got.List[0]
	if len(got.List) != 1 || len(got.NotFound) != 1 || !e.Keywords["$seen"] || e.Keywords["$flagged"] ||
		len(e.MailboxIDs) != 1 || !e.MailboxIDs[archive] {
		t.Fatalf("after set: %+v %+v", e, got.NotFound)
	}

	ch := emailChanges(t, c, s, st, 0).(*email.ChangesResponse)
	if len(ch.Updated) != 1 || ch.Updated[0] != a || len(ch.Destroyed) != 1 || ch.Destroyed[0] != b {
		t.Fatalf("changes after set = %+v", ch)
	}

	stale := do(t, c, &email.Set{Account: s.AccountID(), IfInState: st, Destroy: []jmap.ID{a}})[0].Args
	if err, ok := stale.(*jmap.MethodError); !ok || err.Type != "stateMismatch" {
		t.Fatalf("stale ifInState: %+v", stale)
	}
}

func TestEventSource(t *testing.T) {
	s := jmapmemserver.New("alice", "secret")
	defer s.Close()
	inbox := s.CreateMailbox("Inbox", mailbox.RoleInbox)
	c := newClient(t, s)

	events := make(chan *jmap.StateChange, 10)
	es := &push.EventSource{Client: c, Handler: func(sc *jmap.StateChange) { events <- sc }}
	done := make(chan error, 1)
	go func() { done <- es.Listen() }()
	next := func() *jmap.StateChange {
		t.Helper()
		select {
		case sc := <-events:
			return sc
		case <-time.After(5 * time.Second):
			t.Fatal("no state event")
			return nil
		}
	}

	initial := next() // sent on connect
	s.AddEmail([]jmap.ID{inbox}, []byte(msg1))
	sc := next()
	if sc.Type != "StateChange" || sc.Changed[s.AccountID()]["Email"] == initial.Changed[s.AccountID()]["Email"] {
		t.Fatalf("state change = %+v (initial %+v)", sc, initial)
	}

	s.DropEventSources()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Listen did not return after DropEventSources")
	}
}

func TestMergeEmails(t *testing.T) {
	s := jmapmemserver.New("alice", "secret")
	defer s.Close()
	inbox := s.CreateMailbox("Inbox", mailbox.RoleInbox)
	archive := s.CreateMailbox("Archive", mailbox.RoleArchive)
	a := s.AddEmail([]jmap.ID{inbox}, []byte(msg1), "$seen")
	b := s.AddEmail([]jmap.ID{archive}, []byte(msg1), "$flagged")
	c := newClient(t, s)
	st := do(t, c, &email.Get{Account: s.AccountID(), IDs: []jmap.ID{a}})[0].Args.(*email.GetResponse).State

	merged := s.MergeEmails(a, b)
	if merged == a || merged == b {
		t.Fatalf("merged id %q reuses an old id", merged)
	}
	ch := emailChanges(t, c, s, st, 0).(*email.ChangesResponse)
	if len(ch.Created) != 1 || ch.Created[0] != merged || len(ch.Destroyed) != 2 {
		t.Fatalf("changes after merge = %+v", ch)
	}
	e := do(t, c, &email.Get{Account: s.AccountID(), IDs: []jmap.ID{merged}})[0].Args.(*email.GetResponse).List[0]
	if !e.MailboxIDs[inbox] || !e.MailboxIDs[archive] || !e.Keywords["$seen"] || !e.Keywords["$flagged"] || e.Subject != "café" {
		t.Fatalf("merged email = %+v", e)
	}
}

func TestRejectNextRequest(t *testing.T) {
	s := jmapmemserver.New("alice", "secret")
	defer s.Close()
	c := newClient(t, s)
	s.RejectNextRequest()
	if _, err := c.Do(&jmap.Request{Calls: []*jmap.Invocation{{Name: "Mailbox/get", Args: &mailbox.Get{Account: s.AccountID()}, CallID: "0"}}}); err == nil {
		t.Fatal("request after RejectNextRequest succeeded")
	}
	do(t, c, &mailbox.Get{Account: s.AccountID()}) // only the next one fails

	bearer := (&jmap.Client{SessionEndpoint: s.URL() + "/jmap/session"}).WithAccessToken("secret")
	if err := bearer.Authenticate(); err != nil {
		t.Fatalf("bearer auth: %v", err)
	}
	wrong := (&jmap.Client{SessionEndpoint: s.URL() + "/jmap/session"}).WithBasicAuth("alice", "nope")
	if err := wrong.Authenticate(); err == nil {
		t.Fatal("wrong password accepted")
	}
}

func TestSeparateDownloadHost(t *testing.T) {
	s := jmapmemserver.New("alice", "secret")
	defer s.Close()
	inbox := s.CreateMailbox("Inbox", mailbox.RoleInbox)
	id := s.AddEmail([]jmap.ID{inbox}, []byte(msg1))
	dl := s.SeparateDownloadHost()

	c := newClient(t, s)
	got, _ := url.Parse(c.Session.DownloadURL)
	want, _ := url.Parse(dl)
	api, _ := url.Parse(c.Session.APIURL)
	if got.Host != want.Host || got.Host == api.Host {
		t.Fatalf("downloadUrl %q, download host %q, api %q", c.Session.DownloadURL, dl, c.Session.APIURL)
	}
	e := do(t, c, &email.Get{Account: s.AccountID(), IDs: []jmap.ID{id}})[0].Args.(*email.GetResponse).List[0]
	rc, err := c.Download(s.AccountID(), e.BlobID)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if b, _ := io.ReadAll(rc); string(b) != msg1 {
		t.Fatalf("blob = %q", b)
	}
}

func TestRedirect(t *testing.T) {
	s := jmapmemserver.New("alice", "secret")
	defer s.Close()
	s.Redirect("/.well-known/jmap", "https://elsewhere.example/jmap")
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noFollow.Get(s.URL() + "/.well-known/jmap")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect || resp.Header.Get("Location") != "https://elsewhere.example/jmap" {
		t.Fatalf("got %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	s.Redirect("/.well-known/jmap", "")
	newClient(t, s)
}
