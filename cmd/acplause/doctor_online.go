package main

import (
	"context"
	"errors"
	"time"

	"github.com/anesthetised/acplause/internal/llm"
)

// checkCatalog wraps one existing Models call, including its retries and body
// decoding, in a shared deadline. The function seam keeps tests on local peers
// and permits controlled parent cancellation without altering client policy.
func checkCatalog(parent context.Context, list func(context.Context) ([]llm.Model, error)) (int, string) {
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	models, err := list(ctx)
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		return 0, catalogFailure(err)
	}
	// Match agent.newCatalog: nonempty IDs, Supports("tools"), distinct IDs.
	// Missing supported_parameters means unknown/assumed supported; [] does not.
	selectable := map[string]struct{}{}
	for _, model := range models {
		if model.ID != "" && model.Supports("tools") {
			selectable[model.ID] = struct{}{}
		}
	}
	if len(selectable) == 0 {
		return 0, "no selectable tool-capable models"
	}
	return len(selectable), ""
}

// catalogFailure uses only typed categories. Provider/transport error messages
// can contain URLs, credentials or raw response bodies and must not be printed.
func catalogFailure(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	if api, ok := errors.AsType[*llm.Error](err); ok {
		switch api.Kind {
		case llm.KindAuth:
			return "authentication failed"
		case llm.KindRateLimited:
			return "rate limited"
		case llm.KindTimeout:
			return "timeout"
		case llm.KindUnreachable:
			return "connectivity failed"
		case llm.KindProtocol:
			return "malformed catalog"
		case llm.KindServer:
			return "provider unavailable"
		case llm.KindNotFound:
			return "catalog endpoint unavailable"
		case llm.KindPaymentRequired:
			return "provider billing restriction"
		case llm.KindBadRequest:
			return "provider rejected catalog request"
		}
	}
	return "catalog request failed"
}
