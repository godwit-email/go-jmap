package jmapmemserver

import (
	"net/http/httptest"

	"git.sr.ht/~rockorager/go-jmap"
	"git.sr.ht/~rockorager/go-jmap/mail/email"
	"git.sr.ht/~rockorager/go-jmap/mail/identity"
)

// MergeEmails replaces the given emails with one new email under a new id,
// as servers do when they merge duplicates: the old ids are destroyed and
// the new one created. The result has the first email's content and the
// union of all mailboxIds and keywords. It panics on an unknown id.
func (s *Server) MergeEmails(ids ...jmap.ID) jmap.ID {
	s.mu.Lock()
	defer s.mu.Unlock()
	var merged email.Email
	merged.MailboxIDs, merged.Keywords = map[jmap.ID]bool{}, map[string]bool{}
	for i, id := range ids {
		e := s.email(id)
		if e == nil {
			panic("jmapmemserver: MergeEmails: no email " + string(id))
		}
		if i == 0 {
			mb, kw := merged.MailboxIDs, merged.Keywords
			merged = *e
			merged.MailboxIDs, merged.Keywords = mb, kw
		}
		for k := range e.MailboxIDs {
			merged.MailboxIDs[k] = true
		}
		for k := range e.Keywords {
			merged.Keywords[k] = true
		}
		s.destroyEmail(id)
	}
	merged.ID = s.newID("e")
	merged.ThreadID = merged.ID
	s.emails = append(s.emails, &merged)
	s.record("Email", merged.ID, created)
	return merged.ID
}

// RejectNextRequest makes the next HTTP request, whatever its path or
// credentials, fail with 401 Unauthorized.
func (s *Server) RejectNextRequest() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rejectNext = true
}

// SetPassword changes the server's accepted password or Bearer token.
func (s *Server) SetPassword(password string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.password = password
}

// SeparateDownloadHost starts a second listener and points the session's
// downloadUrl at it, so downloads go to a different host:port than the API.
// It returns the new host's base URL. Close stops it too.
func (s *Server) SeparateDownloadHost() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dlSrv == nil {
		s.dlSrv = httptest.NewServer(s)
		s.sessionState++
	}
	return s.dlSrv.URL
}

// Redirect answers requests for path with 307 Temporary Redirect to
// location, before authentication. An empty location removes the redirect.
func (s *Server) Redirect(path, location string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if location == "" {
		delete(s.redirects, path)
	} else {
		s.redirects[path] = location
	}
}

// SeparateUploadHost starts a second listener and points the session's
// uploadUrl at it, so uploads go to a different host:port than the API.
// It returns the new host's base URL. Close stops it too.
func (s *Server) SeparateUploadHost() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.upSrv == nil {
		s.upSrv = httptest.NewServer(s)
		s.sessionState++
	}
	return s.upSrv.URL
}

// DisableSubmission removes urn:ietf:params:jmap:submission from the session
// and causes Identity/get and EmailSubmission/set to fail.
func (s *Server) DisableSubmission() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.noSubmission = true
	s.sessionState++
}

// SetIdentities replaces the account's sending identities with one Identity
// per email address (for example "alice@example.com", "*@example.com", or "*").
func (s *Server) SetIdentities(emails ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.identities = nil
	for _, addr := range emails {
		s.identities = append(s.identities, &identity.Identity{
			ID:    s.newID("i"),
			Name:  s.username,
			Email: addr,
		})
	}
	s.typeState["Identity"] = s.modseq + 1
	s.modseq++
}

// RefuseRecipient marks an envelope recipient address to be rejected with
// invalidRecipients by EmailSubmission/set.
func (s *Server) RefuseRecipient(rcpt string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refusedRcpts[rcpt] = true
}

// FailNextSubmission makes the next EmailSubmission/set creation fail with
// err in notCreated.
func (s *Server) FailNextSubmission(err *jmap.SetError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNextSub = err
}

// LoseNextSubmission makes the next HTTP request carrying EmailSubmission/set
// process the submission on the server and then drop the TCP connection
// without writing an HTTP response.
func (s *Server) LoseNextSubmission() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loseNextSub = true
}

// StatusNextSubmission makes the next HTTP request carrying
// EmailSubmission/set reply with the given HTTP status code instead of a JMAP
// response.
func (s *Server) StatusNextSubmission(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statusNextSub = code
}
