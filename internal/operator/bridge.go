// bridge.go — GAP-130: adapt the operator registry to the auth.OperatorSource
// seam the daemon's credential path consumes.
//
// The indirection exists so internal/operator can import internal/auth (for
// nothing today, but the role vocabulary mirrors it) while internal/auth
// never imports internal/operator: the daemon wires a *Manager into
// JWTAuth.SetOperatorSource through THIS method set, and the compile-time
// assertion below is what guarantees the shapes never drift.
package operator

import "fmt"

// ValidateToken implements auth.OperatorSource: constant-time hash compare
// of the presented bearer token against every identity, returning the
// matched identity's name, RBAC role and agent binding. Expired identities
// do not validate (see Manager.Validate).
func (m *Manager) ValidateToken(token string) (name, role, agentID string, err error) {
	ident, err := m.Validate(token)
	if err != nil {
		return "", "", "", err
	}
	return ident.Name, string(ident.Role), ident.AgentID, nil
}

// MustValidateToken is ValidateToken for callers that treat a validation
// failure as unreachable (none in production; kept for tests).
func (m *Manager) MustValidateToken(token string) (name, role, agentID string) {
	name, role, agentID, err := m.ValidateToken(token)
	if err != nil {
		panic(fmt.Sprintf("operator: MustValidateToken: %v", err))
	}
	return name, role, agentID
}
