package jmapmemserver

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"git.sr.ht/~rockorager/go-jmap"
	jmail "git.sr.ht/~rockorager/go-jmap/mail"
	"git.sr.ht/~rockorager/go-jmap/mail/email"
)

// AddEmail delivers raw (an RFC 5322 message) into the given mailboxes with
// the given keywords, as if it arrived now, and returns its id.
func (s *Server) AddEmail(mailboxIDs []jmap.ID, raw []byte, keywords ...string) jmap.ID {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := parseEmail(raw)
	e.ID = s.newID("e")
	e.ThreadID = e.ID
	e.BlobID = s.newID("b")
	s.blobs[e.BlobID] = raw
	e.MailboxIDs = map[jmap.ID]bool{}
	for _, id := range mailboxIDs {
		e.MailboxIDs[id] = true
	}
	e.Keywords = map[string]bool{}
	for _, k := range keywords {
		e.Keywords[k] = true
	}
	now := time.Now().UTC()
	e.ReceivedAt = &now
	s.emails = append(s.emails, e)
	s.record("Email", e.ID, created)
	s.typeState["EmailDelivery"] = s.modseq
	return e.ID
}

// parseEmail fills the header-derived properties; bodies are only served as
// the raw blob.
func parseEmail(raw []byte) *email.Email {
	e := &email.Email{Size: uint64(len(raw))}
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return e
	}
	h := m.Header
	dec := new(mime.WordDecoder)
	if subj, err := dec.DecodeHeader(h.Get("Subject")); err == nil {
		e.Subject = subj
	}
	addrs := func(key string) []*jmail.Address {
		list, _ := h.AddressList(key)
		var out []*jmail.Address
		for _, a := range list {
			out = append(out, &jmail.Address{Name: a.Name, Email: a.Address})
		}
		return out
	}
	e.From, e.To, e.CC, e.ReplyTo, e.Sender = addrs("From"), addrs("To"), addrs("Cc"), addrs("Reply-To"), addrs("Sender")
	e.MessageID, e.InReplyTo, e.References = msgIDs(h.Get("Message-Id")), msgIDs(h.Get("In-Reply-To")), msgIDs(h.Get("References"))
	if d, err := h.Date(); err == nil {
		d = d.UTC()
		e.SentAt = &d
	}
	return e
}

func msgIDs(v string) []string {
	var out []string
	for _, f := range strings.Fields(v) {
		out = append(out, strings.Trim(f, "<>"))
	}
	return out
}

// queryArgs is email.Query with a concrete filter type, since email.Filter
// is an interface the decoder cannot fill. Operators are not supported.
type queryArgs struct {
	email.Query
	Filter *email.FilterCondition `json:"filter,omitempty"`
}

func (f *queryArgs) match(e *email.Email) bool {
	c := f.Filter
	if c == nil {
		return true
	}
	return (c.InMailbox == "" || e.MailboxIDs[c.InMailbox]) &&
		(c.HasKeyword == "" || e.Keywords[c.HasKeyword]) &&
		(c.NotKeyword == "" || !e.Keywords[c.NotKeyword]) &&
		(c.After == nil || !e.ReceivedAt.Before(*c.After)) &&
		(c.Before == nil || e.ReceivedAt.Before(*c.Before))
}

func (s *Server) emailQuery(args *queryArgs) (*email.QueryResponse, *jmap.MethodError) {
	asc := false
	for _, c := range args.Sort {
		if c.Property != "receivedAt" {
			return nil, methodErr("unsupportedSort")
		}
		asc = c.IsAscending
	}
	var ids []jmap.ID
	for _, e := range s.emails {
		if args.match(e) {
			ids = append(ids, e.ID)
		}
	}
	// s.emails is in arrival order, which is receivedAt order.
	if !asc {
		for i, j := 0, len(ids)-1; i < j; i, j = i+1, j-1 {
			ids[i], ids[j] = ids[j], ids[i]
		}
	}
	resp := &email.QueryResponse{Account: accountID, QueryState: s.stateOf("Email"), Position: uint64(args.Position)}
	if args.CalculateTotal {
		resp.Total = uint64(len(ids))
	}
	if args.Position < 0 {
		return nil, methodErr("invalidArguments")
	}
	if int(args.Position) < len(ids) {
		ids = ids[args.Position:]
	} else {
		ids = nil
	}
	if args.Limit > 0 && uint64(len(ids)) > args.Limit {
		ids = ids[:args.Limit]
		resp.Limit = args.Limit
	}
	resp.IDs = ids
	return resp, nil
}

func (s *Server) emailGet(args *email.Get) *email.GetResponse {
	resp := &email.GetResponse{Account: accountID, State: s.stateOf("Email")}
	if args.IDs == nil {
		resp.List = s.emails
		return resp
	}
	for _, id := range args.IDs {
		if e := s.email(id); e != nil {
			resp.List = append(resp.List, e)
		} else {
			resp.NotFound = append(resp.NotFound, id)
		}
	}
	return resp
}

func (s *Server) email(id jmap.ID) *email.Email {
	for _, e := range s.emails {
		if e.ID == id {
			return e
		}
	}
	return nil
}

// serveDownload serves /jmap/download/{accountId}/{blobId}/{name}.
func (s *Server) serveDownload(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/jmap/download/"), "/")
	if len(parts) < 2 || jmap.ID(parts[0]) != accountID {
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	b, ok := s.blobs[jmap.ID(parts[1])]
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	typ := r.URL.Query().Get("type")
	if typ == "" {
		typ = "application/octet-stream"
	}
	w.Header().Set("Content-Type", typ)
	io.Copy(w, bytes.NewReader(b))
}

// resolveRefs replaces "#arg" result references (RFC 8620 section 3.7) in
// raw with the values they point to in earlier responses.
func resolveRefs(raw json.RawMessage, prev []*jmap.Invocation) (json.RawMessage, *jmap.MethodError) {
	var args map[string]json.RawMessage
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, methodErr("invalidArguments")
	}
	changed := false
	for k, v := range args {
		if !strings.HasPrefix(k, "#") {
			continue
		}
		var ref jmap.ResultReference
		if err := json.Unmarshal(v, &ref); err != nil {
			return nil, methodErr("invalidResultReference")
		}
		var val any
		found := false
		for _, inv := range prev {
			if inv.CallID == ref.ResultOf && inv.Name == ref.Name {
				b, _ := json.Marshal(inv.Args)
				json.Unmarshal(b, &val)
				found = true
			}
		}
		if !found {
			return nil, methodErr("invalidResultReference")
		}
		val, ok := pointer(val, ref.Path)
		if !ok {
			return nil, methodErr("invalidResultReference")
		}
		b, _ := json.Marshal(val)
		delete(args, k)
		args[k[1:]] = b
		changed = true
	}
	if !changed {
		return raw, nil
	}
	b, _ := json.Marshal(args)
	return b, nil
}

// pointer evaluates a JSON pointer, where "*" maps over an array and
// flattens nested arrays.
func pointer(v any, path string) (any, bool) {
	if path == "" {
		return v, true
	}
	if !strings.HasPrefix(path, "/") {
		return nil, false
	}
	tok, rest := path[1:], ""
	if i := strings.IndexByte(tok, '/'); i >= 0 {
		tok, rest = tok[:i], tok[i:]
	}
	tok = strings.NewReplacer("~1", "/", "~0", "~").Replace(tok)
	switch x := v.(type) {
	case map[string]any:
		child, ok := x[tok]
		if !ok {
			return nil, false
		}
		return pointer(child, rest)
	case []any:
		if tok != "*" {
			return nil, false
		}
		var out []any
		for _, item := range x {
			r, ok := pointer(item, rest)
			if !ok {
				return nil, false
			}
			if arr, isArr := r.([]any); isArr {
				out = append(out, arr...)
			} else {
				out = append(out, r)
			}
		}
		return out, true
	}
	return nil, false
}

func setErr(typ string, props ...string) *jmap.SetError {
	e := &jmap.SetError{Type: typ}
	if props != nil {
		e.Properties = &props
	}
	return e
}

// emailSet supports updating mailboxIds and keywords, whole or per key, and
// destroy. Create is refused.
func (s *Server) emailSet(args *email.Set) (*email.SetResponse, *jmap.MethodError) {
	old := s.stateOf("Email")
	if args.IfInState != "" && args.IfInState != old {
		return nil, methodErr("stateMismatch")
	}
	resp := &email.SetResponse{Account: accountID, OldState: old}
	for id := range args.Create {
		if resp.NotCreated == nil {
			resp.NotCreated = map[jmap.ID]*jmap.SetError{}
		}
		resp.NotCreated[id] = setErr("forbidden")
	}
	for id, patch := range args.Update {
		if serr := s.updateEmail(id, patch); serr != nil {
			if resp.NotUpdated == nil {
				resp.NotUpdated = map[jmap.ID]*jmap.SetError{}
			}
			resp.NotUpdated[id] = serr
			continue
		}
		if resp.Updated == nil {
			resp.Updated = map[jmap.ID]*email.Email{}
		}
		resp.Updated[id] = nil
	}
	for _, id := range args.Destroy {
		if !s.destroyEmail(id) {
			if resp.NotDestroyed == nil {
				resp.NotDestroyed = map[jmap.ID]*jmap.SetError{}
			}
			resp.NotDestroyed[id] = setErr("notFound")
			continue
		}
		resp.Destroyed = append(resp.Destroyed, id)
	}
	resp.NewState = s.stateOf("Email")
	return resp, nil
}

func (s *Server) updateEmail(id jmap.ID, patch jmap.Patch) *jmap.SetError {
	e := s.email(id)
	if e == nil {
		return setErr("notFound")
	}
	// Patch copies so a bad patch leaves the email untouched.
	mboxes := map[jmap.ID]bool{}
	for k, v := range e.MailboxIDs {
		mboxes[k] = v
	}
	kws := map[string]bool{}
	for k, v := range e.Keywords {
		kws[k] = v
	}
	for path, v := range patch {
		prop, key, sub := strings.Cut(path, "/")
		switch {
		case prop == "mailboxIds" && !sub:
			m, ok := v.(map[string]any)
			if !ok {
				return setErr("invalidProperties", path)
			}
			mboxes = map[jmap.ID]bool{}
			for k := range m {
				mboxes[jmap.ID(k)] = true
			}
		case prop == "mailboxIds":
			if v == true {
				mboxes[jmap.ID(key)] = true
			} else {
				delete(mboxes, jmap.ID(key))
			}
		case prop == "keywords" && !sub:
			m, ok := v.(map[string]any)
			if !ok {
				return setErr("invalidProperties", path)
			}
			kws = map[string]bool{}
			for k := range m {
				kws[k] = true
			}
		case prop == "keywords":
			if v == true {
				kws[key] = true
			} else {
				delete(kws, key)
			}
		default:
			return setErr("invalidProperties", path)
		}
	}
	if len(mboxes) == 0 {
		return setErr("invalidProperties", "mailboxIds")
	}
	for m := range mboxes {
		if s.mailbox(m) == nil {
			return setErr("invalidProperties", "mailboxIds")
		}
	}
	e.MailboxIDs, e.Keywords = mboxes, kws
	s.record("Email", id, updated)
	return nil
}

func (s *Server) destroyEmail(id jmap.ID) bool {
	for i, e := range s.emails {
		if e.ID == id {
			s.emails = append(s.emails[:i:i], s.emails[i+1:]...)
			s.record("Email", id, destroyed)
			return true
		}
	}
	return false
}
