package internal

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/ttab/elephant-api/spell"
	"github.com/ttab/elephant-spell/postgres"
	"github.com/ttab/elephantine"
	"github.com/ttab/elephantine/pg"
	"github.com/twitchtv/twirp"
)

// ruleDataFromRPC builds the stored guard data for a rule, or nil when there
// are no guards.
func ruleDataFromRPC(r *spell.Rule) *postgres.RuleData {
	if len(r.Before) == 0 && len(r.After) == 0 &&
		len(r.NotBefore) == 0 && len(r.NotAfter) == 0 {
		return nil
	}

	return &postgres.RuleData{
		Before:    r.Before,
		After:     r.After,
		NotBefore: r.NotBefore,
		NotAfter:  r.NotAfter,
	}
}

// ListRules implements spell.Rules.
func (a *Application) ListRules(
	ctx context.Context, req *spell.ListRulesRequest,
) (*spell.ListRulesResponse, error) {
	_, err := elephantine.RequireAnyScope(ctx, ScopeSpellcheckWrite)
	if err != nil {
		return nil, err //nolint: wrapcheck
	}

	if strings.Contains(req.Query, "%") {
		return nil, twirp.InvalidArgumentError("query", "query cannot contain '%'")
	}

	var pattern string

	if req.Query != "" {
		pattern = "%" + req.Query + "%"
	}

	limit := req.PageSize
	if limit <= 0 {
		limit = 100
	}

	rows, err := a.q.ListRules(ctx, postgres.ListRulesParams{
		Language: pg.TextOrNull(req.Language),
		Query:    pg.TextOrNull(pattern),
		Status:   pg.TextOrNull(req.Status),
		Limit:    limit,
		Offset:   limit * req.Page,
	})
	if err != nil {
		return nil, twirp.InternalErrorf("read from database: %w", err)
	}

	res := spell.ListRulesResponse{
		Rules: make([]*spell.Rule, len(rows)),
	}

	for i, row := range rows {
		rule, err := ruleToRPC(row)
		if err != nil {
			return nil, twirp.InternalErrorf("convert rule: %v", err)
		}

		res.Rules[i] = rule
	}

	return &res, nil
}

// GetRule implements spell.Rules.
func (a *Application) GetRule(
	ctx context.Context, req *spell.GetRuleRequest,
) (*spell.GetRuleResponse, error) {
	_, err := elephantine.RequireAnyScope(ctx, ScopeSpellcheckWrite)
	if err != nil {
		return nil, err //nolint: wrapcheck
	}

	if req.Id == 0 {
		return nil, twirp.RequiredArgumentError("id")
	}

	row, err := a.q.GetRule(ctx, req.Id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, twirp.NotFoundError("rule does not exist")
	} else if err != nil {
		return nil, twirp.InternalErrorf("read from database: %w", err)
	}

	rule, err := ruleToRPC(row)
	if err != nil {
		return nil, twirp.InternalErrorf("convert rule: %v", err)
	}

	return &spell.GetRuleResponse{Rule: rule}, nil
}

// SetRule implements spell.Rules. A zero id creates a new rule; a non-zero id
// updates the existing rule.
func (a *Application) SetRule(
	ctx context.Context, req *spell.SetRuleRequest,
) (_ *spell.SetRuleResponse, outErr error) {
	auth, err := elephantine.RequireAnyScope(ctx, ScopeSpellcheckWrite)
	if err != nil {
		return nil, err //nolint: wrapcheck
	}

	if req.Rule == nil {
		return nil, twirp.RequiredArgumentError("rule")
	}

	if req.Rule.Language == "" {
		return nil, twirp.RequiredArgumentError("rule.language")
	}

	_, ok := a.languages[req.Rule.Language]
	if !ok {
		return nil, twirp.InvalidArgumentError("rule.language",
			"unknown language")
	}

	if req.Rule.Name == "" {
		return nil, twirp.RequiredArgumentError("rule.name")
	}

	if req.Rule.Status == "" {
		return nil, twirp.RequiredArgumentError("rule.status")
	}

	if req.Rule.Pattern == "" {
		return nil, twirp.RequiredArgumentError("rule.pattern")
	}

	level, err := entryLevelFromRPC(req.Rule.Level)
	if err != nil {
		return nil, err
	}

	// Validate the pattern up front so a broken rule can't be stored.
	_, err = compileRule(RuleDef{Pattern: req.Rule.Pattern})
	if err != nil {
		return nil, twirp.InvalidArgumentError("rule.pattern", err.Error())
	}

	tx, err := a.db.Begin(ctx)
	if err != nil {
		return nil, twirp.InternalErrorf("start transaction: %w", err)
	}

	defer pg.Rollback(tx, &outErr)

	q := a.q.WithTx(tx)
	now := pgtype.Timestamptz{Time: time.Now(), Valid: true}

	id := req.Rule.Id

	if id == 0 {
		id, err = q.InsertRule(ctx, postgres.InsertRuleParams{
			Language:    req.Rule.Language,
			Name:        req.Rule.Name,
			Status:      req.Rule.Status,
			Description: req.Rule.Description,
			Level:       level,
			Pattern:     req.Rule.Pattern,
			Replacement: req.Rule.Replacement,
			Data:        ruleDataFromRPC(req.Rule),
			Updated:     now,
			UpdatedBy:   auth.Claims.Subject,
		})
		if err != nil {
			return nil, twirp.InternalErrorf("write to database: %w", err)
		}
	} else {
		affected, err := q.UpdateRule(ctx, postgres.UpdateRuleParams{
			ID:          id,
			Name:        req.Rule.Name,
			Status:      req.Rule.Status,
			Description: req.Rule.Description,
			Level:       level,
			Pattern:     req.Rule.Pattern,
			Replacement: req.Rule.Replacement,
			Data:        ruleDataFromRPC(req.Rule),
			Updated:     now,
			UpdatedBy:   auth.Claims.Subject,
		})
		if err != nil {
			return nil, twirp.InternalErrorf("write to database: %w", err)
		}

		if affected == 0 {
			return nil, twirp.NotFoundError("rule does not exist")
		}
	}

	err = a.recordChange(ctx, q, tx,
		req.Rule.Language, strconv.FormatInt(id, 10), false, eventKindRule)
	if err != nil {
		return nil, twirp.InternalErrorf("record rule change: %w", err)
	}

	err = tx.Commit(ctx)
	if err != nil {
		return nil, twirp.InternalErrorf("commit changes: %w", err)
	}

	return &spell.SetRuleResponse{Id: id}, nil
}

// SetRuleStatus implements spell.Rules.
func (a *Application) SetRuleStatus(
	ctx context.Context, req *spell.SetRuleStatusRequest,
) (_ *spell.SetRuleStatusResponse, outErr error) {
	auth, err := elephantine.RequireAnyScope(ctx, ScopeSpellcheckWrite)
	if err != nil {
		return nil, err //nolint: wrapcheck
	}

	if req.Id == 0 {
		return nil, twirp.RequiredArgumentError("id")
	}

	if req.Status == "" {
		return nil, twirp.RequiredArgumentError("status")
	}

	tx, err := a.db.Begin(ctx)
	if err != nil {
		return nil, twirp.InternalErrorf("start transaction: %w", err)
	}

	defer pg.Rollback(tx, &outErr)

	q := a.q.WithTx(tx)

	language, err := q.SetRuleStatus(ctx, postgres.SetRuleStatusParams{
		ID:        req.Id,
		Status:    req.Status,
		Updated:   pgtype.Timestamptz{Time: time.Now(), Valid: true},
		UpdatedBy: auth.Claims.Subject,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, twirp.NotFoundError("rule does not exist")
	} else if err != nil {
		return nil, twirp.InternalErrorf("write to database: %w", err)
	}

	err = a.recordChange(ctx, q, tx,
		language, strconv.FormatInt(req.Id, 10), false, eventKindRule)
	if err != nil {
		return nil, twirp.InternalErrorf("record rule change: %w", err)
	}

	err = tx.Commit(ctx)
	if err != nil {
		return nil, twirp.InternalErrorf("commit changes: %w", err)
	}

	return &spell.SetRuleStatusResponse{}, nil
}

// DeleteRule implements spell.Rules.
func (a *Application) DeleteRule(
	ctx context.Context, req *spell.DeleteRuleRequest,
) (_ *spell.DeleteRuleResponse, outErr error) {
	_, err := elephantine.RequireAnyScope(ctx, ScopeSpellcheckWrite)
	if err != nil {
		return nil, err //nolint: wrapcheck
	}

	if req.Id == 0 {
		return nil, twirp.RequiredArgumentError("id")
	}

	tx, err := a.db.Begin(ctx)
	if err != nil {
		return nil, twirp.InternalErrorf("start transaction: %w", err)
	}

	defer pg.Rollback(tx, &outErr)

	q := a.q.WithTx(tx)

	language, err := q.DeleteRule(ctx, req.Id)
	if errors.Is(err, pgx.ErrNoRows) {
		// Nothing to delete — treat as a no-op success.
		if err := tx.Commit(ctx); err != nil {
			return nil, twirp.InternalErrorf("commit changes: %w", err)
		}

		return &spell.DeleteRuleResponse{}, nil
	} else if err != nil {
		return nil, twirp.InternalErrorf("write to database: %w", err)
	}

	err = a.recordChange(ctx, q, tx,
		language, strconv.FormatInt(req.Id, 10), true, eventKindRule)
	if err != nil {
		return nil, twirp.InternalErrorf("record rule change: %w", err)
	}

	err = tx.Commit(ctx)
	if err != nil {
		return nil, twirp.InternalErrorf("commit changes: %w", err)
	}

	return &spell.DeleteRuleResponse{}, nil
}
