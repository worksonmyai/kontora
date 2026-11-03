package web

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// OperatorTokenHeader is separate from the shared web token. Only a credential
// issued to a person may establish the identity used for approval or acceptance.
const OperatorTokenHeader = "X-Kontora-Operator-Token"

type OperatorCredential struct {
	ID    string
	Token string
}

// AttemptScope binds a capability to one execution, not just its ticket or work.
type AttemptScope struct {
	BoardID   string
	WorkID    string
	AttemptID string
}

type AttemptAccess string

const (
	AttemptRead    AttemptAccess = "read"
	AttemptPropose AttemptAccess = "propose"
)

// AttemptCapability grants reads, or reads and proposal submission. Neither
// access level grants ordinary ticket mutations, approval, or acceptance.
type AttemptCapability struct {
	Token     string
	Scope     AttemptScope
	Access    AttemptAccess
	ExpiresAt time.Time
}

// AuthorityConfig opts into operator-only access to /api/ and /ws/, except for
// the narrow attempt routes below. Credentials arrive in headers, never JSON,
// cookies, query parameters, or forwarded identity headers. Callers must keep
// operator credentials out of worker environments and enforce Host/origin checks.
//
// AttemptActive must consult current execution state on every request. It must
// return false for revoked, superseded, or finished attempts. The handler must
// recheck that state atomically with mutations to avoid a revocation race.
type AuthorityConfig struct {
	Operators     []OperatorCredential
	Attempts      []AttemptCapability
	AttemptActive func(context.Context, AttemptScope) bool
}

type authorityIdentity struct {
	operator string
	attempt  AttemptScope
	access   AttemptAccess
}

type authorityContextKey struct{}

// OperatorIdentity returns only the identity established by authority middleware.
// Request bodies, including an operator_id field, cannot populate it.
func OperatorIdentity(ctx context.Context) (string, bool) {
	identity, ok := ctx.Value(authorityContextKey{}).(authorityIdentity)
	return identity.operator, ok && identity.operator != ""
}

func AttemptIdentity(ctx context.Context) (AttemptScope, bool) {
	identity, ok := ctx.Value(authorityContextKey{}).(authorityIdentity)
	return identity.attempt, ok && identity.attempt.AttemptID != ""
}

// Place RequireOperator inside the authority middleware, after authentication.
// It always enforces authority.
// Do not install it on ordinary routes when authority is disabled.
func RequireOperator(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := OperatorIdentity(r.Context()); !ok {
			writeJSONError(w, http.StatusForbidden, "operator authority required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

type authorityCredential struct {
	hash      [sha256.Size]byte
	identity  authorityIdentity
	expiresAt time.Time
}

// AuthorityMiddleware replaces authMiddleware, rather than wrapping it: an
// operator credential or attempt capability authenticates independently of the
// shared token. A nil config preserves ordinary shared-token/open behavior.
//
// Workers may GET /api/boards/{boardID}/attempts/{attemptID}, its /logs and
// /activity subroutes, and (with AttemptPropose) POST its /proposals subroute.
// All other protected routes require an operator, including raw config, both
// terminal modes, assistant gates, and future operator endpoints. These routes
// only authorize transport; they do not install handlers or enable execution.
func AuthorityMiddleware(sharedToken string, cfg *AuthorityConfig, next http.Handler) (http.Handler, error) {
	if cfg == nil {
		return authMiddleware(sharedToken, next), nil
	}
	credentials, err := authorityCredentials(sharedToken, cfg)
	if err != nil {
		return nil, err
	}
	active := cfg.AttemptActive
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !requiresAuth(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		// An explicitly presented bad operator credential must not fall back to
		// a worker capability, cookie, or the shared token.
		operatorToken := r.Header.Get(OperatorTokenHeader)
		presented := operatorToken
		if presented == "" {
			if bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
				presented = bearer
			}
		}
		hash := sha256.Sum256([]byte(presented))
		var identity authorityIdentity
		var expiresAt time.Time
		for _, credential := range credentials {
			match := subtle.ConstantTimeCompare(hash[:], credential.hash[:])
			if match == 1 {
				identity, expiresAt = credential.identity, credential.expiresAt
			}
		}
		isOperator := identity.operator != ""
		isAttempt := identity.attempt.AttemptID != ""
		if presented == "" || (operatorToken != "") != isOperator || (!isOperator && !isAttempt) {
			writeJSONError(w, http.StatusUnauthorized, "authority credential required")
			return
		}
		if isAttempt {
			if !time.Now().Before(expiresAt) || !active(r.Context(), identity.attempt) {
				writeJSONError(w, http.StatusUnauthorized, "attempt capability expired or revoked")
				return
			}
			if !attemptRouteAllowed(r, identity) {
				writeJSONError(w, http.StatusForbidden, "attempt capability does not authorize this route")
				return
			}
		}
		ctx := context.WithValue(r.Context(), authorityContextKey{}, identity)
		next.ServeHTTP(w, r.WithContext(ctx))
	}), nil
}

func authorityCredentials(sharedToken string, cfg *AuthorityConfig) ([]authorityCredential, error) {
	var credentials []authorityCredential
	seenTokens := make(map[[sha256.Size]byte]bool)
	if sharedToken != "" {
		seenTokens[sha256.Sum256([]byte(sharedToken))] = true
	}
	add := func(token string, identity authorityIdentity, expiresAt time.Time) error {
		hash := sha256.Sum256([]byte(token))
		if token == "" || seenTokens[hash] {
			return fmt.Errorf("authority credentials must be nonempty and distinct from each other and the shared token")
		}
		seenTokens[hash] = true
		credentials = append(credentials, authorityCredential{hash: hash, identity: identity, expiresAt: expiresAt})
		return nil
	}
	seenIDs := make(map[string]bool)
	for _, operator := range cfg.Operators {
		if strings.TrimSpace(operator.ID) == "" || seenIDs[operator.ID] {
			return nil, fmt.Errorf("operator identities must be nonempty and unique")
		}
		seenIDs[operator.ID] = true
		if err := add(operator.Token, authorityIdentity{operator: operator.ID}, time.Time{}); err != nil {
			return nil, err
		}
	}
	if len(cfg.Attempts) > 0 && cfg.AttemptActive == nil {
		return nil, fmt.Errorf("attempt capabilities require a current-state validator")
	}
	for _, attempt := range cfg.Attempts {
		if !validAuthorityID(attempt.Scope.BoardID) || strings.TrimSpace(attempt.Scope.WorkID) == "" || !validAuthorityID(attempt.Scope.AttemptID) {
			return nil, fmt.Errorf("attempt capabilities require board, work, and attempt identities")
		}
		if attempt.Access != AttemptRead && attempt.Access != AttemptPropose {
			return nil, fmt.Errorf("invalid attempt capability access")
		}
		if attempt.ExpiresAt.IsZero() {
			return nil, fmt.Errorf("attempt capabilities require an expiry")
		}
		if err := add(attempt.Token, authorityIdentity{attempt: attempt.Scope, access: attempt.Access}, attempt.ExpiresAt); err != nil {
			return nil, err
		}
	}
	return credentials, nil
}

func validAuthorityID(id string) bool {
	return id != "" && id != "." && id != ".." && !strings.ContainsAny(id, "/\\\x00\r\n\t ")
}

func attemptRouteAllowed(r *http.Request, identity authorityIdentity) bool {
	// ServeMux unescapes each segment separately. Splitting URL.Path instead
	// would mistake an encoded slash for a route separator and could authorize
	// a request that the mux delivers to a different, operator-only handler.
	parts := strings.Split(r.URL.EscapedPath(), "/")
	if len(parts) != 6 && len(parts) != 7 {
		return false
	}
	for i, part := range parts {
		decoded, err := url.PathUnescape(part)
		if err != nil {
			return false
		}
		parts[i] = decoded
	}
	if parts[0] != "" || parts[1] != "api" || parts[2] != "boards" || parts[3] != identity.attempt.BoardID || parts[4] != "attempts" || parts[5] != identity.attempt.AttemptID {
		return false
	}
	if r.Method == http.MethodGet {
		return len(parts) == 6 || parts[6] == "logs" || parts[6] == "activity"
	}
	return r.Method == http.MethodPost && len(parts) == 7 && parts[6] == "proposals" && identity.access == AttemptPropose
}
