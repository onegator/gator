package quality

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/process"
	"github.com/onegator/gator/internal/server/store/db"
)

// builtinChecks are the rules the core can answer from what a project has already told it.
// They are deliberately dull: whether a part has an owner, a repository and a description.
// Anything about the code itself — tests, coverage, vulnerable dependencies — belongs to a
// plugin that can actually go and look.
func builtinChecks(components []db.Component) []Check {
	out := make([]Check, 0, len(components)*3)
	for _, c := range components {
		out = append(out,
			Check{Component: c.Key, Rule: "owner named", Source: "builtin", Weight: 2,
				Status: passIf(c.OwnerID.Valid),
				Detail: detail(c.OwnerID.Valid, "", "nobody is named as the owner, so a question about this part has nowhere to go")},
			Check{Component: c.Key, Rule: "repository known", Source: "builtin", Weight: 2,
				Status: passIf(c.Repo != ""),
				Detail: detail(c.Repo != "", "", "no repository, so an agent sent to work here does not know where the code is")},
			Check{Component: c.Key, Rule: "described", Source: "builtin", Weight: 1,
				Status: passIf(len(c.Notes) > 0),
				Detail: detail(len(c.Notes) > 0, "", "no notes: whatever a person knows about this part is not written down anywhere")},
		)
	}
	return out
}

func passIf(ok bool) string {
	if ok {
		return "pass"
	}
	return "fail"
}

func detail(ok bool, whenPass, whenFail string) string {
	if ok {
		return whenPass
	}
	return whenFail
}

// policy reads the project's scorecard settings out of process_config.
func (s *Service) policy(ctx context.Context, projectID pgtype.UUID) (Policy, error) {
	p, err := db.New(s.pool).GetProject(ctx, projectID)
	if err != nil {
		return Policy{}, err
	}
	cfg, err := process.ParseProjectConfig(p.ProcessConfig)
	if err != nil {
		return Policy{}, err
	}
	var out Policy
	if len(cfg.Quality) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(cfg.Quality, &out); err != nil {
		return Policy{}, err
	}
	return out, nil
}
