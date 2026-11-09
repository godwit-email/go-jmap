// Package jmapmemserver implements an in-memory JMAP server for tests.
//
// It serves one account over HTTP (net/http/httptest) and covers the subset
// of RFC 8620 and RFC 8621 a mail client needs to sync: the session resource,
// Mailbox/get, Mailbox/changes, Email/get, Email/query, Email/changes,
// Email/set (mailboxIds, keywords, destroy), blob download and EventSource.
// Hook methods on Server inject server-side changes and faults.
package jmapmemserver

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"

	"git.sr.ht/~rockorager/go-jmap"
	"git.sr.ht/~rockorager/go-jmap/core"
	"git.sr.ht/~rockorager/go-jmap/mail"
	"git.sr.ht/~rockorager/go-jmap/mail/email"
	"git.sr.ht/~rockorager/go-jmap/mail/mailbox"
)

const accountID jmap.ID = "a1"

// Server is an in-memory JMAP server holding a single account.
type Server struct {
	username, password string

	srv *httptest.Server

	mu        sync.Mutex
	nextID    int
	mailboxes []*mailbox.Mailbox
	emails    []*email.Email // in arrival order
	blobs     map[jmap.ID][]byte
	modseq    int
	minSeq    int
	log       []change
	typeState map[string]int
	subs      map[*subscriber]struct{}

	// fault injection, see faults.go
	rejectNext   bool
	dlSrv        *httptest.Server
	sessionState int
	redirects    map[string]string
}

// New starts a server that accepts Basic auth with username and password,
// or Bearer auth with password as the token. Call Close when done.
func New(username, password string) *Server {
	s := &Server{username: username, password: password, blobs: map[jmap.ID][]byte{}, typeState: map[string]int{},
		subs: map[*subscriber]struct{}{}, sessionState: 1, redirects: map[string]string{}}
	s.srv = httptest.NewServer(s)
	return s
}

// URL is the server's base URL; the session resource is at
// URL()+"/.well-known/jmap" and URL()+"/jmap/session".
func (s *Server) URL() string { return s.srv.URL }

// Close shuts the server down.
func (s *Server) Close() {
	s.DropEventSources()
	s.srv.Close()
	if s.dlSrv != nil {
		s.dlSrv.Close()
	}
}

// AccountID is the id of the server's only account.
func (s *Server) AccountID() jmap.ID { return accountID }

func (s *Server) newID(prefix string) jmap.ID {
	s.nextID++
	return jmap.ID(fmt.Sprintf("%s%d", prefix, s.nextID))
}

// CreateMailbox adds a mailbox; role may be empty.
func (s *Server) CreateMailbox(name string, role mailbox.Role) jmap.ID {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := &mailbox.Mailbox{ID: s.newID("m"), Name: name, Role: role}
	s.mailboxes = append(s.mailboxes, m)
	s.record("Mailbox", m.ID, created)
	return m.ID
}

func (s *Server) authorized(r *http.Request) bool {
	if u, p, ok := r.BasicAuth(); ok {
		return eq(u, s.username) && eq(p, s.password)
	}
	h := r.Header.Get("Authorization")
	return strings.HasPrefix(h, "Bearer ") && eq(strings.TrimPrefix(h, "Bearer "), s.password)
}

func eq(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	location, redirect := s.redirects[r.URL.Path]
	reject := s.rejectNext
	s.rejectNext = false
	s.mu.Unlock()
	if redirect {
		http.Redirect(w, r, location, http.StatusTemporaryRedirect)
		return
	}
	if reject || !s.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Basic realm="jmapmemserver"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch {
	case r.URL.Path == "/.well-known/jmap" || r.URL.Path == "/jmap/session":
		s.mu.Lock()
		s.serveSession(w)
		s.mu.Unlock()
	case r.URL.Path == "/jmap/api" && r.Method == http.MethodPost:
		s.serveAPI(w, r)
	case r.URL.Path == "/jmap/eventsource":
		s.serveEventSource(w, r)
	case strings.HasPrefix(r.URL.Path, "/jmap/download/"):
		s.serveDownload(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) serveSession(w http.ResponseWriter) {
	dl := s.srv.URL
	if s.dlSrv != nil {
		dl = s.dlSrv.URL
	}
	raw := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
	caps := map[jmap.URI]json.RawMessage{
		jmap.CoreURI: raw(&core.Core{
			MaxSizeUpload: 50 << 20, MaxConcurrentUpload: 4, MaxSizeRequest: 10 << 20,
			MaxConcurrentRequests: 4, MaxCallsInRequest: 16, MaxObjectsInGet: 500, MaxObjectsInSet: 500,
			CollationAlgorithms: []jmap.CollationAlgo{jmap.ASCIICasemap},
		}),
		mail.URI: raw(&mail.Mail{MaxSizeMailboxName: 255, EmailQuerySortOptions: []string{"receivedAt"}}),
	}
	sess := jmap.Session{
		RawCapabilities: caps,
		Accounts: map[jmap.ID]jmap.Account{accountID: {
			Name: s.username, IsPersonal: true,
			RawCapabilities: map[jmap.URI]json.RawMessage{mail.URI: caps[mail.URI]},
		}},
		PrimaryAccounts: map[jmap.URI]jmap.ID{mail.URI: accountID},
		Username:        s.username,
		APIURL:          s.srv.URL + "/jmap/api",
		DownloadURL:     dl + "/jmap/download/{accountId}/{blobId}/{name}?type={type}",
		UploadURL:       s.srv.URL + "/jmap/upload/{accountId}",
		EventSourceURL:  s.srv.URL + "/jmap/eventsource?types={types}&closeafter={closeafter}&ping={ping}",
		State:           strconv.Itoa(s.sessionState),
	}
	writeJSON(w, &sess)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

type apiRequest struct {
	Using       []jmap.URI           `json:"using"`
	MethodCalls [][3]json.RawMessage `json:"methodCalls"`
}

func (s *Server) serveAPI(w http.ResponseWriter, r *http.Request) {
	var req apiRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(&jmap.RequestError{
			Type: "urn:ietf:params:jmap:error:notRequest", Status: 400, Detail: err.Error(),
		})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	resp := jmap.Response{SessionState: strconv.Itoa(s.sessionState)}
	for _, call := range req.MethodCalls {
		var name, callID string
		json.Unmarshal(call[0], &name)
		json.Unmarshal(call[2], &callID)
		var args any
		raw, err := resolveRefs(call[1], resp.Responses)
		if err == nil {
			args, err = s.call(name, raw)
		}
		if err != nil {
			name, args = "error", err
		}
		resp.Responses = append(resp.Responses, &jmap.Invocation{Name: name, Args: args, CallID: callID})
	}
	writeJSON(w, &resp)
}

func methodErr(typ string) *jmap.MethodError { return &jmap.MethodError{Type: typ} }

// call runs one method with the lock held.
func (s *Server) call(name string, raw json.RawMessage) (any, *jmap.MethodError) {
	var acct struct {
		AccountID jmap.ID `json:"accountId"`
	}
	if err := json.Unmarshal(raw, &acct); err != nil {
		return nil, methodErr("invalidArguments")
	}
	if acct.AccountID != accountID {
		return nil, methodErr("accountNotFound")
	}
	switch name {
	case "Email/query":
		var args queryArgs
		if json.Unmarshal(raw, &args) != nil {
			return nil, methodErr("invalidArguments")
		}
		return s.emailQuery(&args)
	case "Email/get":
		var args email.Get
		if json.Unmarshal(raw, &args) != nil {
			return nil, methodErr("invalidArguments")
		}
		return s.emailGet(&args), nil
	case "Email/set":
		var args email.Set
		if json.Unmarshal(raw, &args) != nil {
			return nil, methodErr("invalidArguments")
		}
		return s.emailSet(&args)
	case "Email/changes":
		var args email.Changes
		if json.Unmarshal(raw, &args) != nil {
			return nil, methodErr("invalidArguments")
		}
		cs, err := s.changes("Email", args.SinceState, args.MaxChanges)
		if err != nil {
			return nil, err
		}
		return &email.ChangesResponse{Account: accountID, OldState: cs.oldState, NewState: cs.newState,
			HasMoreChanges: cs.hasMore, Created: cs.created, Updated: cs.updated, Destroyed: cs.destroyed}, nil
	case "Mailbox/changes":
		var args mailbox.Changes
		if json.Unmarshal(raw, &args) != nil {
			return nil, methodErr("invalidArguments")
		}
		cs, err := s.changes("Mailbox", args.SinceState, args.MaxChanges)
		if err != nil {
			return nil, err
		}
		return &mailbox.ChangesResponse{Account: accountID, OldState: cs.oldState, NewState: cs.newState,
			HasMoreChanges: cs.hasMore, Created: cs.created, Updated: cs.updated, Destroyed: cs.destroyed}, nil
	case "Mailbox/get":
		var args mailbox.Get
		if json.Unmarshal(raw, &args) != nil {
			return nil, methodErr("invalidArguments")
		}
		return s.mailboxGet(&args), nil
	default:
		return nil, methodErr("unknownMethod")
	}
}

func (s *Server) mailboxGet(args *mailbox.Get) *mailbox.GetResponse {
	resp := &mailbox.GetResponse{Account: accountID, State: s.stateOf("Mailbox")}
	if args.IDs == nil {
		resp.List = s.mailboxes
		return resp
	}
	for _, id := range args.IDs {
		if m := s.mailbox(id); m != nil {
			resp.List = append(resp.List, m)
		} else {
			resp.NotFound = append(resp.NotFound, string(id))
		}
	}
	return resp
}

func (s *Server) mailbox(id jmap.ID) *mailbox.Mailbox {
	for _, m := range s.mailboxes {
		if m.ID == id {
			return m
		}
	}
	return nil
}
