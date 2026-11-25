package brain

import "strings"

// Kinds of sources.
const (
	SourceManual  = "manual"  // a person wrote it
	SourceMessage = "message" // said in a conversation
	SourceTask    = "task"    // came out of a task's work (a gate, a rollback, a curation)
	SourceJob     = "job"     // an agent proposed it from inside a job
	SourcePlugin  = "plugin"  // a plugin proposed it (an ADR in a repository, a recurring error)
	SourceImport  = "import"  // carried over from before the Brain
	SourcePack    = "pack"    // a knowledge pack's file: read-only
)

// maxExcerpt bounds the quote a source carries.
const maxExcerpt = 500

// SourceRule checks and tidies one kind of source before it is stored.
type SourceRule func(Source) (Source, error)

// Sources are the kinds of origin the Brain accepts, by name. A new kind is one more entry here,
// not another branch somewhere.
type Sources map[string]SourceRule

// DefaultSources are the kinds Gator itself writes.
func DefaultSources() Sources {
	return Sources{
		SourceManual:  anything,
		SourceImport:  anything,
		SourceMessage: needs("id", func(s Source) string { return s.ID }),
		SourceTask:    needs("id", func(s Source) string { return s.ID }),
		SourceJob:     needs("id", func(s Source) string { return s.ID }),
		SourcePlugin:  needs("name", func(s Source) string { return s.Name }),
		SourcePack:    needs("name", func(s Source) string { return s.Name }),
	}
}

// Check answers src as it is kept, or why it is refused.
func (ss Sources) Check(src Source) (Source, error) {
	src.Kind = strings.TrimSpace(src.Kind)
	if src.Kind == "" {
		src.Kind = SourceManual
	}
	rule, ok := ss[src.Kind]
	if !ok {
		return Source{}, invalid("a source is one of the kinds the Brain knows, not %q", src.Kind)
	}
	src.Excerpt = clip(src.Excerpt, maxExcerpt)
	return rule(src)
}

// ReadOnly says whether entries from this source are kept as they came: a pack's file changes in
// its pack, not here.
func ReadOnly(s Source) bool { return s.Kind == SourcePack }

func anything(s Source) (Source, error) { return s, nil }

func needs(field string, get func(Source) string) SourceRule {
	return func(s Source) (Source, error) {
		if strings.TrimSpace(get(s)) == "" {
			return Source{}, invalid("a %s source names its %s", s.Kind, field)
		}
		return s, nil
	}
}
