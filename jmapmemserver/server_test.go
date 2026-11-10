package jmapmemserver_test

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"git.sr.ht/~rockorager/go-jmap"
	"git.sr.ht/~rockorager/go-jmap/core/push"
	"git.sr.ht/~rockorager/go-jmap/jmapmemserver"
	"git.sr.ht/~rockorager/go-jmap/mail/email"
	"git.sr.ht/~rockorager/go-jmap/mail/emailsubmission"
	"git.sr.ht/~rockorager/go-jmap/mail/identity"
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

func TestEmptyQueryFeedsResultReference(t *testing.T) {
	s := jmapmemserver.New("alice", "secret")
	defer s.Close()
	inbox := s.CreateMailbox("Inbox", mailbox.RoleInbox)

	c := newClient(t, s)
	req := &jmap.Request{}
	q := req.Invoke(&email.Query{Account: s.AccountID(), Filter: &email.FilterCondition{InMailbox: inbox}})
	req.Invoke(&email.Get{Account: s.AccountID(), ReferenceIDs: &jmap.ResultReference{ResultOf: q, Name: "Email/query", Path: "/ids"}})
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if gr, ok := resp.Responses[1].Args.(*email.GetResponse); !ok || len(gr.List) != 0 {
		t.Fatalf("Email/get = %#v", resp.Responses[1].Args)
	}
}

func TestUploadAndImport(t *testing.T) {
	s := jmapmemserver.New("alice", "secret")
	defer s.Close()
	sent := s.CreateMailbox("Sent", mailbox.RoleSent)

	c := newClient(t, s)
	up, err := c.Upload(s.AccountID(), strings.NewReader(msg1))
	if err != nil {
		t.Fatal(err)
	}
	if up.Account != s.AccountID() || up.ID == "" || up.Size != uint64(len(msg1)) {
		t.Fatalf("upload = %+v", up)
	}
	at := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	ir := do(t, c, &email.Import{Account: s.AccountID(), Emails: map[string]*email.EmailImport{
		"k1": {BlobID: up.ID, MailboxIDs: map[jmap.ID]bool{sent: true}, Keywords: map[string]bool{"$seen": true}, ReceivedAt: &at},
		"k2": {BlobID: "nope", MailboxIDs: map[jmap.ID]bool{sent: true}},
		"k3": {BlobID: up.ID, MailboxIDs: map[jmap.ID]bool{"nope": true}},
	}})[0].Args.(*email.ImportResponse)
	if ir.NotCreated["k2"] == nil || ir.NotCreated["k2"].Type != "blobNotFound" ||
		ir.NotCreated["k3"] == nil || ir.NotCreated["k3"].Type != "invalidProperties" {
		t.Fatalf("notCreated = %+v", ir.NotCreated)
	}
	created := ir.Created["k1"]
	if created == nil || created.ID == "" || created.BlobID != up.ID || ir.OldState == ir.NewState {
		t.Fatalf("Email/import = %+v", ir)
	}

	e := do(t, c, &email.Get{Account: s.AccountID(), IDs: []jmap.ID{created.ID}})[0].Args.(*email.GetResponse).List[0]
	if !e.MailboxIDs[sent] || !e.Keywords["$seen"] || !e.ReceivedAt.Equal(at) || e.Subject != "café" {
		t.Fatalf("imported email = %+v", e)
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

func TestIdentityAndEmailSubmission(t *testing.T) {
	s := jmapmemserver.New("alice@example.com", "secret")
	defer s.Close()
	drafts := s.CreateMailbox("Drafts", mailbox.RoleDrafts)
	sent := s.CreateMailbox("Sent", mailbox.RoleSent)
	upHost := s.SeparateUploadHost()

	c := (&jmap.Client{SessionEndpoint: s.URL() + "/.well-known/jmap"}).WithBasicAuth("alice@example.com", "secret")
	if err := c.Authenticate(); err != nil {
		t.Fatal(err)
	}
	gotUp, _ := url.Parse(c.Session.UploadURL)
	wantUp, _ := url.Parse(upHost)
	if gotUp.Host != wantUp.Host {
		t.Fatalf("uploadUrl %q, want host %q", c.Session.UploadURL, wantUp.Host)
	}

	ids := do(t, c, &identity.Get{Account: s.AccountID()})[0].Args.(*identity.GetResponse)
	if len(ids.List) != 1 || ids.List[0].Email != "alice@example.com" {
		t.Fatalf("Identity/get = %+v", ids.List)
	}
	idID := ids.List[0].ID

	const outMsg = "From: Alice <alice@example.com>\r\nTo: bob@example.com\r\nSubject: hi\r\n\r\nhello\r\n"
	up, err := c.Upload(s.AccountID(), strings.NewReader(outMsg))
	if err != nil {
		t.Fatal(err)
	}

	req := &jmap.Request{}
	req.Invoke(&email.Import{Account: s.AccountID(), Emails: map[string]*email.EmailImport{
		"m": {BlobID: up.ID, MailboxIDs: map[jmap.ID]bool{drafts: true}, Keywords: map[string]bool{"$seen": true, "$draft": true}},
	}})
	subCall := req.Invoke(&emailsubmission.Set{
		Account: s.AccountID(),
		Create: map[jmap.ID]*emailsubmission.EmailSubmission{
			"s": {
				IdentityID: idID,
				EmailID:    "#m",
				Envelope: &emailsubmission.Envelope{
					MailFrom: &emailsubmission.Address{Email: "alice@example.com"},
					RcptTo: []*emailsubmission.Address{
						{Email: "bob@example.com"},
						{Email: "bcc@example.com"},
					},
				},
			},
		},
		OnSuccessUpdateEmail: map[jmap.ID]jmap.Patch{
			"#s": {
				"mailboxIds/" + string(drafts): nil,
				"mailboxIds/" + string(sent):   true,
				"keywords/$draft":              nil,
			},
		},
	})
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Responses) != 3 {
		t.Fatalf("got %d responses, want 3", len(resp.Responses))
	}
	ir := resp.Responses[0].Args.(*email.ImportResponse)
	emailID := ir.Created["m"].ID
	sr := resp.Responses[1].Args.(*emailsubmission.SetResponse)
	if sr.Created["s"] == nil || sr.Created["s"].ID == "" {
		t.Fatalf("EmailSubmission/set = %+v", sr)
	}
	if resp.Responses[2].Name != "Email/set" || resp.Responses[2].CallID != subCall {
		t.Fatalf("implicit Email/set invocation = %+v", resp.Responses[2])
	}
	es := resp.Responses[2].Args.(*email.SetResponse)
	if _, ok := es.Updated[emailID]; !ok {
		t.Fatalf("implicit Email/set updated = %+v", es)
	}

	stored := do(t, c, &email.Get{Account: s.AccountID(), IDs: []jmap.ID{emailID}})[0].Args.(*email.GetResponse).List[0]
	if stored.MailboxIDs[drafts] || !stored.MailboxIDs[sent] || stored.Keywords["$draft"] || !stored.Keywords["$seen"] {
		t.Fatalf("stored email after send = %+v", stored)
	}
	subs := s.Submissions()
	if len(subs) != 1 || subs[0].MailFrom != "alice@example.com" || len(subs[0].RcptTo) != 2 ||
		subs[0].RcptTo[1] != "bcc@example.com" || string(subs[0].Raw) != outMsg {
		t.Fatalf("submissions = %+v", subs)
	}

	// Refused recipient returns invalidRecipients and leaves email untouched.
	s.RefuseRecipient("bad@example.com")
	up2, _ := c.Upload(s.AccountID(), strings.NewReader(outMsg))
	req2 := &jmap.Request{}
	req2.Invoke(&email.Import{Account: s.AccountID(), Emails: map[string]*email.EmailImport{
		"m": {BlobID: up2.ID, MailboxIDs: map[jmap.ID]bool{drafts: true}, Keywords: map[string]bool{"$draft": true}},
	}})
	req2.Invoke(&emailsubmission.Set{
		Account: s.AccountID(),
		Create: map[jmap.ID]*emailsubmission.EmailSubmission{
			"s": {
				IdentityID: idID,
				EmailID:    "#m",
				Envelope: &emailsubmission.Envelope{
					MailFrom: &emailsubmission.Address{Email: "alice@example.com"},
					RcptTo:   []*emailsubmission.Address{{Email: "bad@example.com"}},
				},
			},
		},
		OnSuccessUpdateEmail: map[jmap.ID]jmap.Patch{"#s": {"keywords/$draft": nil}},
	})
	resp2, err := c.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp2.Responses) != 2 {
		t.Fatalf("failed submission got %d responses, want 2", len(resp2.Responses))
	}
	sr2 := resp2.Responses[1].Args.(*emailsubmission.SetResponse)
	if sr2.NotCreated["s"] == nil || sr2.NotCreated["s"].Type != "invalidRecipients" {
		t.Fatalf("refused submission = %+v", sr2.NotCreated)
	}
	if len(s.Submissions()) != 1 {
		t.Fatalf("submissions count = %d, want 1", len(s.Submissions()))
	}

	// LoseNextSubmission accepts the submission and drops the HTTP response.
	s.LoseNextSubmission()
	up3, _ := c.Upload(s.AccountID(), strings.NewReader(outMsg))
	req3 := &jmap.Request{}
	req3.Invoke(&email.Import{Account: s.AccountID(), Emails: map[string]*email.EmailImport{
		"m": {BlobID: up3.ID, MailboxIDs: map[jmap.ID]bool{sent: true}, Keywords: map[string]bool{"$seen": true}},
	}})
	req3.Invoke(&emailsubmission.Set{
		Account: s.AccountID(),
		Create: map[jmap.ID]*emailsubmission.EmailSubmission{
			"s": {
				IdentityID: idID,
				EmailID:    "#m",
				Envelope: &emailsubmission.Envelope{
					MailFrom: &emailsubmission.Address{Email: "alice@example.com"},
					RcptTo:   []*emailsubmission.Address{{Email: "bob@example.com"}},
				},
			},
		},
	})
	if _, err := c.Do(req3); err == nil {
		t.Fatal("LoseNextSubmission returned a response, want dropped connection")
	}
	if len(s.Submissions()) != 2 {
		t.Fatalf("submissions count after LoseNextSubmission = %d, want 2", len(s.Submissions()))
	}

	// DisableSubmission removes the capability from the session.
	s.DisableSubmission()
	if err := c.Authenticate(); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Session.RawCapabilities[emailsubmission.URI]; ok {
		t.Fatal("submission capability still present after DisableSubmission")
	}
}
