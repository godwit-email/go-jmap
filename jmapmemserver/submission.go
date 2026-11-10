package jmapmemserver

import (
	"bytes"
	"strings"

	"git.sr.ht/~rockorager/go-jmap"
	"git.sr.ht/~rockorager/go-jmap/mail/email"
	"git.sr.ht/~rockorager/go-jmap/mail/emailsubmission"
	"git.sr.ht/~rockorager/go-jmap/mail/identity"
)

// Submission records an email accepted by EmailSubmission/set.
type Submission struct {
	ID         jmap.ID
	IdentityID jmap.ID
	EmailID    jmap.ID
	MailFrom   string
	RcptTo     []string
	Raw        []byte
}

// Submissions returns a copy of all accepted submissions in order.
func (s *Server) Submissions() []Submission {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Submission, len(s.submissions))
	for i, sub := range s.submissions {
		sub.RcptTo = append([]string(nil), sub.RcptTo...)
		sub.Raw = append([]byte(nil), sub.Raw...)
		out[i] = sub
	}
	return out
}

func (s *Server) identityGet(args *identity.Get) *identity.GetResponse {
	resp := &identity.GetResponse{Account: accountID, State: s.stateOf("Identity")}
	if args.IDs == nil {
		resp.List = append([]*identity.Identity(nil), s.identities...)
		return resp
	}
	for _, id := range args.IDs {
		if got := s.identity(id); got != nil {
			resp.List = append(resp.List, got)
		} else {
			resp.NotFound = append(resp.NotFound, id)
		}
	}
	return resp
}

func (s *Server) identity(id jmap.ID) *identity.Identity {
	for _, got := range s.identities {
		if got.ID == id {
			return got
		}
	}
	return nil
}

func setErrDesc(typ, desc string) *jmap.SetError {
	return &jmap.SetError{Type: typ, Description: &desc}
}

func resolveCreationID(id jmap.ID, createdIDs map[jmap.ID]jmap.ID) jmap.ID {
	if rest, ok := strings.CutPrefix(string(id), "#"); ok {
		if real, exists := createdIDs[jmap.ID(rest)]; exists {
			return real
		}
	}
	return id
}

func identityAllows(pattern, addr string) bool {
	if pattern == "*" || pattern == "*@*" || pattern == "" {
		return true
	}
	if dom, ok := strings.CutPrefix(pattern, "*@"); ok {
		_, addrDom, hasAt := strings.Cut(addr, "@")
		return hasAt && strings.EqualFold(dom, addrDom)
	}
	return strings.EqualFold(pattern, addr)
}

// emailSubmissionSet implements EmailSubmission/set (RFC 8621 section 7.5).
func (s *Server) emailSubmissionSet(args *emailsubmission.Set, callID string, createdIDs map[jmap.ID]jmap.ID) (*emailsubmission.SetResponse, []*jmap.Invocation, *jmap.MethodError) {
	old := s.stateOf("EmailSubmission")
	if args.IfInState != "" && args.IfInState != old {
		return nil, nil, methodErr("stateMismatch")
	}
	resp := &emailsubmission.SetResponse{Account: accountID, OldState: old}
	notCreated := func(cid jmap.ID, serr *jmap.SetError) {
		if resp.NotCreated == nil {
			resp.NotCreated = map[jmap.ID]*jmap.SetError{}
		}
		resp.NotCreated[cid] = serr
	}

	succeeded := map[jmap.ID]jmap.ID{} // submission creationID -> resolved emailID
	for cid, sub := range args.Create {
		if s.failNextSub != nil {
			serr := s.failNextSub
			s.failNextSub = nil
			notCreated(cid, serr)
			continue
		}
		if sub == nil {
			notCreated(cid, setErr("invalidProperties"))
			continue
		}
		id := s.identity(sub.IdentityID)
		if id == nil {
			notCreated(cid, setErr("invalidProperties", "identityId"))
			continue
		}
		emailID := resolveCreationID(sub.EmailID, createdIDs)
		e := s.email(emailID)
		if e == nil {
			notCreated(cid, setErr("invalidProperties", "emailId"))
			continue
		}

		var mailFrom string
		var rcptTo []string
		if sub.Envelope != nil {
			if sub.Envelope.MailFrom != nil {
				mailFrom = sub.Envelope.MailFrom.Email
			}
			for _, r := range sub.Envelope.RcptTo {
				if r != nil && r.Email != "" {
					rcptTo = append(rcptTo, r.Email)
				}
			}
		} else {
			if len(e.Sender) > 0 {
				mailFrom = e.Sender[0].Email
			} else if len(e.From) > 0 {
				mailFrom = e.From[0].Email
			}
			for _, a := range e.To {
				rcptTo = append(rcptTo, a.Email)
			}
			for _, a := range e.CC {
				rcptTo = append(rcptTo, a.Email)
			}
			for _, a := range e.BCC {
				rcptTo = append(rcptTo, a.Email)
			}
		}

		if mailFrom != "" && !identityAllows(id.Email, mailFrom) {
			notCreated(cid, setErrDesc("forbiddenMailFrom", "identity cannot send from "+mailFrom))
			continue
		}
		if len(e.From) > 0 && !identityAllows(id.Email, e.From[0].Email) {
			notCreated(cid, setErrDesc("forbiddenFrom", "identity cannot send From "+e.From[0].Email))
			continue
		}
		if len(rcptTo) == 0 {
			notCreated(cid, setErrDesc("noRecipients", "envelope has no recipients"))
			continue
		}
		var bad []string
		for _, r := range rcptTo {
			if s.refusedRcpts[r] {
				bad = append(bad, r)
			}
		}
		if len(bad) > 0 {
			desc := "recipient refused: " + strings.Join(bad, ", ")
			notCreated(cid, &jmap.SetError{
				Type:              "invalidRecipients",
				Description:       &desc,
				InvalidRecipients: &bad,
			})
			continue
		}

		raw := stripBccHeader(s.blobs[e.BlobID])
		subID := s.newID("sub")
		if createdIDs != nil {
			createdIDs[cid] = subID
		}
		s.submissions = append(s.submissions, Submission{
			ID:         subID,
			IdentityID: id.ID,
			EmailID:    e.ID,
			MailFrom:   mailFrom,
			RcptTo:     rcptTo,
			Raw:        raw,
		})
		s.record("EmailSubmission", subID, created)
		if resp.Created == nil {
			resp.Created = map[jmap.ID]*emailsubmission.EmailSubmission{}
		}
		resp.Created[cid] = &emailsubmission.EmailSubmission{
			ID:         subID,
			ThreadID:   e.ThreadID,
			UndoStatus: "final",
		}
		succeeded[cid] = e.ID
	}

	for id := range args.Update {
		if resp.NotUpdated == nil {
			resp.NotUpdated = map[jmap.ID]*jmap.SetError{}
		}
		resp.NotUpdated[id] = setErr("forbidden")
	}
	for _, id := range args.Destroy {
		if resp.NotDestroyed == nil {
			resp.NotDestroyed = map[jmap.ID]*jmap.SetError{}
		}
		resp.NotDestroyed[id] = setErr("forbidden")
	}
	resp.NewState = s.stateOf("EmailSubmission")

	var extra []*jmap.Invocation
	if len(succeeded) > 0 && (len(args.OnSuccessUpdateEmail) > 0 || len(args.OnSuccessDestroyEmail) > 0) {
		var eset email.Set
		eset.Account = accountID
		for k, patch := range args.OnSuccessUpdateEmail {
			ref, ok := strings.CutPrefix(string(k), "#")
			if !ok {
				continue
			}
			emailID, ok := succeeded[jmap.ID(ref)]
			if !ok {
				continue
			}
			if eset.Update == nil {
				eset.Update = map[jmap.ID]jmap.Patch{}
			}
			eset.Update[emailID] = patch
		}
		for _, k := range args.OnSuccessDestroyEmail {
			ref, ok := strings.CutPrefix(string(k), "#")
			if !ok {
				continue
			}
			if emailID, ok := succeeded[jmap.ID(ref)]; ok {
				eset.Destroy = append(eset.Destroy, emailID)
			}
		}
		if len(eset.Update) > 0 || len(eset.Destroy) > 0 {
			esResp, merr := s.emailSet(&eset)
			if merr != nil {
				extra = append(extra, &jmap.Invocation{Name: "error", Args: merr, CallID: callID})
			} else {
				extra = append(extra, &jmap.Invocation{Name: "Email/set", Args: esResp, CallID: callID})
			}
		}
	}
	return resp, extra, nil
}

// stripBccHeader removes top-level Bcc header fields from an RFC 5322 message
// when Envelope is nil or before relaying, as described in RFC 8621 §7.5.
func stripBccHeader(raw []byte) []byte {
	sep := []byte("\r\n\r\n")
	idx := bytes.Index(raw, sep)
	lineSep := "\r\n"
	if idx < 0 {
		sep = []byte("\n\n")
		idx = bytes.Index(raw, sep)
		lineSep = "\n"
	}
	if idx < 0 {
		return append([]byte(nil), raw...)
	}
	hdr, body := string(raw[:idx]), raw[idx:]
	lines := strings.Split(hdr, lineSep)
	var kept []string
	dropping := false
	for _, line := range lines {
		if len(line) > 0 && (line[0] == ' ' || line[0] == '\t') {
			if !dropping {
				kept = append(kept, line)
			}
			continue
		}
		name, _, ok := strings.Cut(line, ":")
		dropping = ok && strings.EqualFold(strings.TrimSpace(name), "Bcc")
		if !dropping {
			kept = append(kept, line)
		}
	}
	out := make([]byte, 0, len(raw))
	out = append(out, strings.Join(kept, lineSep)...)
	out = append(out, body...)
	return out
}
