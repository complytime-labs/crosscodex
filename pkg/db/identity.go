package db

// Identity lifecycle (#195, migration 009): tenants and reviewers are
// soft-deleted, can be hidden behind a pseudonym, and every change or trace
// appends an identity_events row carrying the signed request. TenantAdmin is
// the only Go code that calls these SECURITY DEFINER functions.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

const (
	// TenantStatusDeleted is the status of a soft-deleted tenant. Only
	// delete_tenant sets it; set_tenant_status refuses it as a target.
	TenantStatusDeleted = "deleted"

	IdentityKindTenant   = "tenant"
	IdentityKindReviewer = "reviewer"

	// MaxReasonBytes bounds --reason, as record_identity_event does.
	MaxReasonBytes = 500
	// MaxReviewerBytes bounds a reviewer ID and --by, as the reviewers CHECK
	// and record_identity_event do.
	MaxReviewerBytes = 256
)

// pseudonymPattern matches the pseudonyms hide_tenant and hide_reviewer
// generate, and the CHECK constraints on tenants and reviewers.
var pseudonymPattern = regexp.MustCompile(`^hidden-(tenant|reviewer)-[0-9a-f]{16}$`)

// ErrIdentityState reports an action the identity's current state forbids
// (SQLSTATE 55000), such as hiding an active tenant. The wrapped message says
// what to do instead.
var ErrIdentityState = errors.New("identity is not in a state that allows this")

// ErrReviewerNotFound reports a reviewer with no reviewers row.
var ErrReviewerNotFound = errors.New("reviewer not registered")

// ErrPseudonymNotFound reports a trace of a pseudonym no identity holds.
var ErrPseudonymNotFound = errors.New("no identity has this pseudonym")

// IdentityRequest is what an operator asked for. Its canonical JSON is
// stored in identity_events.request and is the material an in-toto link
// signs, so a verifier needs only the row and the public key.
// record_identity_event refuses a request whose action, kind, tenant_id,
// identity_id, actor or reason differ from the call, so a signed request
// cannot be replayed against another identity.
type IdentityRequest struct {
	Action string `json:"action"`
	// Kind and TenantID are empty for a trace: the operator does not know
	// them until the trace answers.
	Kind     string `json:"kind"`
	TenantID string `json:"tenant_id"`
	// IdentityID is the tenant or reviewer ID, or the pseudonym for a trace.
	IdentityID  string    `json:"identity_id"`
	Actor       string    `json:"actor"`
	Reason      string    `json:"reason"`
	RequestedAt time.Time `json:"-"`
}

// Canonical returns the request as JSON with fixed key order and the time
// in UTC RFC 3339, so this encoder always produces the same bytes for the
// same request. That holds only for this Go encoder: a verifier must hash
// the request bytes stored in identity_events, never a re-encoding. A zero
// RequestedAt is refused, since the signed time would be meaningless.
func (r IdentityRequest) Canonical() ([]byte, error) {
	if r.RequestedAt.IsZero() {
		return nil, errors.New("encode identity request: requested_at is not set; set RequestedAt to the time the operator asked")
	}
	b, err := json.Marshal(struct {
		IdentityRequest
		RequestedAt string `json:"requested_at"`
	}{r, r.RequestedAt.UTC().Format(time.RFC3339Nano)})
	if err != nil {
		return nil, fmt.Errorf("encode identity request: %w", err)
	}
	return b, nil
}

// SignedRequest is a canonical request and the link envelope signing it.
type SignedRequest struct {
	Request     []byte
	Attestation []byte
}

// ReviewerRecord is one reviewers row as list_reviewers() returns it.
type ReviewerRecord struct {
	ID        string // the pseudonym when Hidden
	Status    string
	CreatedAt time.Time
	CreatedBy string
	DeletedAt *time.Time
	Hidden    bool
}

// TraceResult is the identity behind a pseudonym.
type TraceResult struct {
	Kind, TenantID, IdentityID, Status string
	Hidden                             bool
}

// ValidateReason checks --reason, which is stored in identity_events and
// shown in audits.
func ValidateReason(reason string) error {
	return textRule{name: "--reason", maxBytes: MaxReasonBytes, plain: "plain text",
		emptyHint: "say why this change is made; it is kept in the audit log"}.check(reason)
}

// ValidateReviewerID checks a human reviewer ID before it is registered or
// stored as determined_by in PostgreSQL and on graph edges. It is the only
// Go check of reviewer IDs; the reviewers CHECK constraints back it up.
func ValidateReviewerID(id string) error {
	return textRule{name: "reviewer", maxBytes: MaxReviewerBytes, plain: "a plain-text ID", exact: true,
		emptyHint: "pass the ID of the person recording this verdict; it is stored as determined_by"}.check(id)
}

// ValidateActor checks --by, the operator recorded in identity_events. An
// operator ID follows the same rules as a reviewer ID, which are at least as
// strict as record_identity_event's.
func ValidateActor(actor string) error {
	return textRule{name: "--by", maxBytes: MaxReviewerBytes, plain: "a plain-text ID", exact: true,
		emptyHint: "name the operator making this change; it is kept in the audit log"}.check(actor)
}

// textRule describes operator-supplied text that is stored and later
// printed: not blank, at most maxBytes, valid UTF-8, and no control, format
// or line/paragraph separator characters, which could corrupt terminals or
// reorder and hide text. exact also refuses leading or trailing spaces, for
// IDs that are compared byte for byte. name and the hints fill the messages.
type textRule struct {
	name, emptyHint, plain string
	maxBytes               int
	exact                  bool
}

func (t textRule) check(s string) error {
	switch {
	case strings.TrimSpace(s) == "":
		return fmt.Errorf("%s is empty: %s", t.name, t.emptyHint)
	case len(s) > t.maxBytes:
		return fmt.Errorf("%s is %d bytes: use at most %d bytes", t.name, len(s), t.maxBytes)
	case !utf8.ValidString(s):
		return fmt.Errorf("%s is not valid UTF-8: use %s", t.name, t.plain)
	}
	for _, r := range s {
		if unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp) {
			return fmt.Errorf("%s contains control, format or line-separator characters (%U): use %s", t.name, r, t.plain)
		}
	}
	if t.exact && strings.TrimSpace(s) != s {
		return fmt.Errorf("%s has leading or trailing spaces: pass the ID exactly as it should be stored", t.name)
	}
	return nil
}

// ValidatePseudonym checks a pseudonym passed to Trace.
func ValidatePseudonym(p string) error {
	if !pseudonymPattern.MatchString(p) {
		return errors.New("pseudonym must look like hidden-tenant-<16 hex> or hidden-reviewer-<16 hex>; copy it exactly from the output that showed it")
	}
	return nil
}

// validateReviewerCall checks a reviewer action's arguments.
func validateReviewerCall(tenantID, reviewer, actor, reason string) error {
	if err := validateIdentityCall(tenantID, actor, reason); err != nil {
		return err
	}
	return ValidateReviewerID(reviewer)
}

// validateIdentityCall checks the arguments every identity action shares.
func validateIdentityCall(tenantID, actor, reason string) error {
	if err := tenant.ValidateTenantID(tenantID); err != nil {
		return err
	}
	if err := ValidateActor(actor); err != nil {
		return err
	}
	return ValidateReason(reason)
}

// identityErr maps the SQLSTATEs the identity functions raise to sentinels;
// the database messages already say what to do, so they are kept verbatim.
// notFound is the sentinel for P0002 on this operation's kind.
func identityErr(op string, notFound, err error) error {
	switch {
	case err == nil:
		return nil
	case IsPgErrorCode(err, "P0002"): // no_data_found
		return fmt.Errorf("%s: %w: %w", op, notFound, err)
	case IsPgErrorCode(err, "55000"): // object_not_in_prerequisite_state
		return fmt.Errorf("%s: %w: %w", op, ErrIdentityState, err)
	default:
		return fmt.Errorf("%s: %w", op, err)
	}
}

// DeleteTenant soft-deletes a tenant: its data and judgements stay, and
// every app path refuses it.
func (a *TenantAdmin) DeleteTenant(ctx context.Context, id, actor, reason string, s SignedRequest) (err error) {
	ctx, done := a.tel.op(ctx, "delete_tenant")
	defer func() { done(err) }()
	if err := validateIdentityCall(id, actor, reason); err != nil {
		return err
	}
	setTenantAttribute(ctx, id)
	err = a.pool.Exec(ctx, "SELECT public.delete_tenant($1, $2, $3, $4, $5)", id, actor, reason, s.Request, s.Attestation)
	return identityErr(fmt.Sprintf("delete tenant %q", id), ErrTenantNotFound, err)
}

// RestoreTenant brings a deleted, unhidden tenant back as suspended, so an
// operator re-activates it deliberately with SetStatus.
func (a *TenantAdmin) RestoreTenant(ctx context.Context, id, actor, reason string, s SignedRequest) (err error) {
	ctx, done := a.tel.op(ctx, "restore_tenant")
	defer func() { done(err) }()
	if err := validateIdentityCall(id, actor, reason); err != nil {
		return err
	}
	setTenantAttribute(ctx, id)
	err = a.pool.Exec(ctx, "SELECT public.restore_tenant($1, $2, $3, $4, $5)", id, actor, reason, s.Request, s.Attestation)
	return identityErr(fmt.Sprintf("restore tenant %q", id), ErrTenantNotFound, err)
}

// HideTenant hides a deleted tenant behind a pseudonym, which it returns.
func (a *TenantAdmin) HideTenant(ctx context.Context, id, actor, reason string, s SignedRequest) (pseudonym string, err error) {
	ctx, done := a.tel.op(ctx, "hide_tenant")
	defer func() { done(err) }()
	if err := validateIdentityCall(id, actor, reason); err != nil {
		return "", err
	}
	setTenantAttribute(ctx, id)
	err = a.pool.QueryRow(ctx, "SELECT public.hide_tenant($1, $2, $3, $4, $5)", id, actor, reason, s.Request, s.Attestation).
		Scan(&pseudonym)
	if err != nil {
		return "", identityErr(fmt.Sprintf("hide tenant %q", id), ErrTenantNotFound, err)
	}
	return pseudonym, nil
}

// UnhideTenant shows a hidden tenant by its ID again.
func (a *TenantAdmin) UnhideTenant(ctx context.Context, id, actor, reason string, s SignedRequest) (err error) {
	ctx, done := a.tel.op(ctx, "unhide_tenant")
	defer func() { done(err) }()
	if err := validateIdentityCall(id, actor, reason); err != nil {
		return err
	}
	setTenantAttribute(ctx, id)
	err = a.pool.Exec(ctx, "SELECT public.unhide_tenant($1, $2, $3, $4, $5)", id, actor, reason, s.Request, s.Attestation)
	return identityErr(fmt.Sprintf("unhide tenant %q", id), ErrTenantNotFound, err)
}

// AddReviewer registers a reviewer for a tenant, or does nothing if they
// are already active; created reports whether it inserted.
func (a *TenantAdmin) AddReviewer(ctx context.Context, tenantID, reviewer, actor, reason string, s SignedRequest) (created bool, err error) {
	ctx, done := a.tel.op(ctx, "add_reviewer")
	defer func() { done(err) }()
	if err := validateReviewerCall(tenantID, reviewer, actor, reason); err != nil {
		return false, err
	}
	setTenantAttribute(ctx, tenantID)
	err = a.pool.QueryRow(ctx, "SELECT public.add_reviewer($1, $2, $3, $4, $5, $6)",
		tenantID, reviewer, actor, reason, s.Request, s.Attestation).Scan(&created)
	if err != nil {
		// add_reviewer raises P0002 only for an unknown tenant.
		return false, identityErr(fmt.Sprintf("add reviewer %q to tenant %q", reviewer, tenantID), ErrTenantNotFound, err)
	}
	return created, nil
}

// DeleteReviewer soft-deletes a reviewer: their verdicts stay, and new
// verdicts by them are refused.
func (a *TenantAdmin) DeleteReviewer(ctx context.Context, tenantID, reviewer, actor, reason string, s SignedRequest) (err error) {
	ctx, done := a.tel.op(ctx, "delete_reviewer")
	defer func() { done(err) }()
	if err := validateReviewerCall(tenantID, reviewer, actor, reason); err != nil {
		return err
	}
	setTenantAttribute(ctx, tenantID)
	err = a.pool.Exec(ctx, "SELECT public.delete_reviewer($1, $2, $3, $4, $5, $6)",
		tenantID, reviewer, actor, reason, s.Request, s.Attestation)
	return identityErr(fmt.Sprintf("delete reviewer %q of tenant %q", reviewer, tenantID), ErrReviewerNotFound, err)
}

// RestoreReviewer makes a deleted, unhidden reviewer active again.
func (a *TenantAdmin) RestoreReviewer(ctx context.Context, tenantID, reviewer, actor, reason string, s SignedRequest) (err error) {
	ctx, done := a.tel.op(ctx, "restore_reviewer")
	defer func() { done(err) }()
	if err := validateReviewerCall(tenantID, reviewer, actor, reason); err != nil {
		return err
	}
	setTenantAttribute(ctx, tenantID)
	err = a.pool.Exec(ctx, "SELECT public.restore_reviewer($1, $2, $3, $4, $5, $6)",
		tenantID, reviewer, actor, reason, s.Request, s.Attestation)
	return identityErr(fmt.Sprintf("restore reviewer %q of tenant %q", reviewer, tenantID), ErrReviewerNotFound, err)
}

// HideReviewer hides a deleted reviewer behind a pseudonym, which it returns.
func (a *TenantAdmin) HideReviewer(ctx context.Context, tenantID, reviewer, actor, reason string, s SignedRequest) (pseudonym string, err error) {
	ctx, done := a.tel.op(ctx, "hide_reviewer")
	defer func() { done(err) }()
	if err := validateReviewerCall(tenantID, reviewer, actor, reason); err != nil {
		return "", err
	}
	setTenantAttribute(ctx, tenantID)
	err = a.pool.QueryRow(ctx, "SELECT public.hide_reviewer($1, $2, $3, $4, $5, $6)",
		tenantID, reviewer, actor, reason, s.Request, s.Attestation).Scan(&pseudonym)
	if err != nil {
		return "", identityErr(fmt.Sprintf("hide reviewer %q of tenant %q", reviewer, tenantID), ErrReviewerNotFound, err)
	}
	return pseudonym, nil
}

// UnhideReviewer shows a hidden reviewer by their ID again.
func (a *TenantAdmin) UnhideReviewer(ctx context.Context, tenantID, reviewer, actor, reason string, s SignedRequest) (err error) {
	ctx, done := a.tel.op(ctx, "unhide_reviewer")
	defer func() { done(err) }()
	if err := validateReviewerCall(tenantID, reviewer, actor, reason); err != nil {
		return err
	}
	setTenantAttribute(ctx, tenantID)
	err = a.pool.Exec(ctx, "SELECT public.unhide_reviewer($1, $2, $3, $4, $5, $6)",
		tenantID, reviewer, actor, reason, s.Request, s.Attestation)
	return identityErr(fmt.Sprintf("unhide reviewer %q of tenant %q", reviewer, tenantID), ErrReviewerNotFound, err)
}

// ListReviewers returns a tenant's reviewers ordered by ID; a hidden
// reviewer appears only by pseudonym. An unknown tenant has none.
func (a *TenantAdmin) ListReviewers(ctx context.Context, tenantID string) (records []ReviewerRecord, err error) {
	ctx, done := a.tel.op(ctx, "list_reviewers")
	defer func() { done(err) }()
	if err := tenant.ValidateTenantID(tenantID); err != nil {
		return nil, err
	}
	setTenantAttribute(ctx, tenantID)
	op := fmt.Sprintf("list reviewers of tenant %q", tenantID)
	rows, err := a.pool.Query(ctx,
		"SELECT reviewer_id, status, created_at, created_by, deleted_at, hidden FROM public.list_reviewers($1)", tenantID)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	defer func() {
		err = errors.Join(err, rows.Close())
		if err != nil {
			records = nil
		}
	}()
	for rows.Next() {
		var r ReviewerRecord
		if err := rows.Scan(&r.ID, &r.Status, &r.CreatedAt, &r.CreatedBy, &r.DeletedAt, &r.Hidden); err != nil {
			return nil, fmt.Errorf("%s: read row: %w", op, err)
		}
		records = append(records, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	return records, nil
}

// Trace returns the identity behind a pseudonym and records the lookup,
// since revealing a hidden identity is itself audited. The span carries no
// tenant: the trace backend must not learn what the pseudonym hides.
func (a *TenantAdmin) Trace(ctx context.Context, pseudonym, actor, reason string, s SignedRequest) (result TraceResult, err error) {
	ctx, done := a.tel.op(ctx, "trace")
	defer func() { done(err) }()
	if err := ValidatePseudonym(pseudonym); err != nil {
		return TraceResult{}, err
	}
	if err := ValidateActor(actor); err != nil {
		return TraceResult{}, err
	}
	if err := ValidateReason(reason); err != nil {
		return TraceResult{}, err
	}
	err = a.pool.QueryRow(ctx,
		"SELECT kind, tenant_id, identity_id, status, hidden FROM public.trace_pseudonym($1, $2, $3, $4, $5)",
		pseudonym, actor, reason, s.Request, s.Attestation).
		Scan(&result.Kind, &result.TenantID, &result.IdentityID, &result.Status, &result.Hidden)
	if err != nil {
		return TraceResult{}, identityErr(fmt.Sprintf("trace pseudonym %q", pseudonym), ErrPseudonymNotFound, err)
	}
	return result, nil
}
