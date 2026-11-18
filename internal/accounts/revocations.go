package accounts

// Revocations is told when access ends before its time: an account is
// disabled, or an API token deleted. Long-lived connections made with that
// access (tunnels, DESIGN.md "Tunnels") end then; requests are checked on
// their own.
type Revocations interface {
	EndUser(userID string)
	EndToken(tokenID string)
}

// SetRevocations connects the service to whatever must hear about ended
// access. Without it nothing is told.
func (s *Service) SetRevocations(r Revocations) { s.revocations = r }

func (s *Service) userDisabled(userID string) {
	if s.revocations != nil {
		s.revocations.EndUser(userID)
	}
}

func (s *Service) tokenDeleted(tokenID string) {
	if s.revocations != nil {
		s.revocations.EndToken(tokenID)
	}
}
