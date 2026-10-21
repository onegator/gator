package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"strings"
	"text/tabwriter"
	"time"
)

func printJSON(env *Env, v any) error {
	enc := json.NewEncoder(env.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func table(env *Env) *tabwriter.Writer {
	return tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
}

// resolveProject finds a project by slug (or id), so a person never has to type a UUID.
func resolveProject(ctx context.Context, c *Client, ref string) (Project, error) {
	projects, err := c.Projects(ctx)
	if err != nil {
		return Project{}, err
	}
	for _, p := range projects {
		if p.Slug == ref || p.ID == ref {
			return p, nil
		}
	}
	return Project{}, coded(exitUserErr, "no project %q among the ones you can see; gator projects lists them", ref)
}

// projectRef is --project, defaulting to `gator use`'s choice.
func (s *session) projectRef(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if s.cfg.Server == s.server {
		return s.cfg.Project
	}
	return ""
}

func cmdProjects(ctx context.Context, env *Env, args []string) error {
	fs, g := newFlags("projects")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	s, err := open(env, g)
	if err != nil {
		return err
	}
	projects, err := s.client.Projects(ctx)
	if err != nil {
		return err
	}
	if g.json {
		return printJSON(env, projects)
	}
	if len(projects) == 0 {
		fmt.Fprintln(env.Stdout, "No projects yet. Start one with gator project create SLUG --repo owner/repo.")
		return nil
	}
	current := s.projectRef("")
	tw := table(env)
	fmt.Fprintln(tw, "\tSLUG\tNAME\tROLE")
	for _, p := range projects {
		mark := ""
		if p.Slug == current {
			mark = "*"
		}
		role := orDefault(p.Role, "-")
		if p.ArchivedAt != nil {
			role += " (archived)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", mark, p.Slug, p.Name, role)
	}
	return tw.Flush()
}

func cmdProjectCreate(ctx context.Context, env *Env, args []string) error {
	fs, g := newFlags("project create")
	name := fs.String("name", "", "what people call it; the slug when omitted")
	repo := fs.String("repo", "", "its repository: owner/repo, https://, git@host:path")
	branch := fs.String("branch", "", "the branch work starts from; main when omitted")
	positional, err := parse(fs, args)
	if err != nil {
		return err
	}
	slug, err := oneArg(positional, "project short name (slug)")
	if err != nil {
		return err
	}
	if *branch != "" && *repo == "" {
		return coded(exitUserErr, "--branch belongs to a repository; give --repo too")
	}
	s, err := open(env, g)
	if err != nil {
		return err
	}
	in := NewProject{Slug: slug, Name: orDefault(*name, slug)}
	if *repo != "" {
		in.RepoURL = repo
	}
	if *branch != "" {
		in.DefaultBranch = branch
	}
	p, err := s.client.CreateProject(ctx, in)
	if err != nil {
		return err
	}
	// The first project becomes the current one: that is what the person will work on next.
	adopted := false
	if s.projectRef("") == "" && s.cfg.Server == s.server {
		s.cfg.Project = p.Slug
		adopted = SaveConfig(env.ConfigDir, s.cfg) == nil
	}
	if g.json {
		return printJSON(env, p)
	}
	fmt.Fprintf(env.Stdout, "Created project %s (%s); you are its %s.\n", p.Slug, p.Name, orDefault(p.Role, "admin"))
	if adopted {
		fmt.Fprintf(env.Stdout, "It is now the current project; gator tasks and gator task new use it.\n")
	}
	return nil
}

func cmdUse(ctx context.Context, env *Env, args []string) error {
	fs, g := newFlags("use")
	positional, err := parse(fs, args)
	if err != nil {
		return err
	}
	s, err := open(env, g)
	if err != nil {
		return err
	}
	if len(positional) == 0 {
		current := s.projectRef("")
		if g.json {
			return printJSON(env, map[string]string{"project": current})
		}
		if current == "" {
			fmt.Fprintln(env.Stdout, "No current project; choose one with gator use SLUG.")
		} else {
			fmt.Fprintln(env.Stdout, current)
		}
		return nil
	}
	ref, err := oneArg(positional, "project")
	if err != nil {
		return err
	}
	p, err := resolveProject(ctx, s.client, ref)
	if err != nil {
		return err
	}
	s.cfg.Server, s.cfg.Project = s.server, p.Slug
	if err := SaveConfig(env.ConfigDir, s.cfg); err != nil {
		return err
	}
	if g.json {
		return printJSON(env, p)
	}
	fmt.Fprintf(env.Stdout, "Now using %s (%s).\n", p.Slug, p.Name)
	return nil
}

func cmdTasks(ctx context.Context, env *Env, args []string) error {
	fs, g := newFlags("tasks")
	project := fs.String("project", "", "project slug; the current project when omitted, every project when there is none")
	mine := fs.Bool("mine", false, "only tasks handed to you")
	all := fs.Bool("all", false, "include closed tasks")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	s, err := open(env, g)
	if err != nil {
		return err
	}
	projects, err := s.client.Projects(ctx)
	if err != nil {
		return err
	}
	ref := s.projectRef(*project)
	if ref != "" {
		p, err := resolveProject(ctx, s.client, ref)
		if err != nil {
			return err
		}
		projects = []Project{p}
	}
	var me Me
	if *mine {
		if me, err = s.client.Me(ctx); err != nil {
			return err
		}
	}
	type row struct {
		Task
		ProjectSlug string `json:"projectSlug"`
	}
	var rows []row
	for _, p := range projects {
		if ref == "" && p.ArchivedAt != nil {
			continue
		}
		tasks, err := s.client.Tasks(ctx, p.ID)
		if err != nil {
			return err
		}
		for _, t := range tasks {
			if !*all && t.ClosedAt != nil {
				continue
			}
			if *mine && (t.OwnerKind != "user" || t.OwnerID != me.UserID) {
				continue
			}
			rows = append(rows, row{Task: t, ProjectSlug: p.Slug})
		}
	}
	if g.json {
		if rows == nil {
			rows = []row{}
		}
		return printJSON(env, rows)
	}
	if len(rows) == 0 {
		fmt.Fprintln(env.Stdout, "No tasks.")
		return nil
	}
	tw := table(env)
	if ref == "" {
		fmt.Fprintln(tw, "ID\tPROJECT\tKIND\tPHASE\tTITLE")
	} else {
		fmt.Fprintln(tw, "ID\tKIND\tPHASE\tTITLE")
	}
	for _, r := range rows {
		phase := r.Phase
		if r.BlockedReason != "" {
			phase += " (blocked)"
		}
		if ref == "" {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.ID, r.ProjectSlug, r.Kind, phase, r.Title)
		} else {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.ID, r.Kind, phase, r.Title)
		}
	}
	return tw.Flush()
}

func cmdTaskShow(ctx context.Context, env *Env, args []string) error {
	fs, g := newFlags("task show")
	positional, err := parse(fs, args)
	if err != nil {
		return err
	}
	id, err := oneArg(positional, "task id")
	if err != nil {
		return err
	}
	s, err := open(env, g)
	if err != nil {
		return err
	}
	t, err := s.client.Task(ctx, id)
	if err != nil {
		return err
	}
	jobs, err := s.client.TaskJobs(ctx, id)
	if err != nil {
		return err
	}
	if g.json {
		if jobs == nil {
			jobs = []Job{}
		}
		return printJSON(env, struct {
			TaskDetail
			Jobs []Job `json:"jobs"`
		}{t, jobs})
	}
	w := env.Stdout
	fmt.Fprintf(w, "%s\n%s · %s · urgency %d\n", t.Title, t.Kind, t.ID, t.Urgency)
	if len(t.Phases) > 0 {
		marked := make([]string, len(t.Phases))
		for i, p := range t.Phases {
			marked[i] = p
			if p == t.Phase {
				marked[i] = "[" + p + "]"
			}
		}
		fmt.Fprintf(w, "Phase: %s\n", strings.Join(marked, " → "))
	} else {
		fmt.Fprintf(w, "Phase: %s\n", t.Phase)
	}
	if t.ClosedAt != nil {
		fmt.Fprintf(w, "Closed %s\n", t.ClosedAt.Local().Format(time.DateTime))
	}
	if t.BlockedReason != "" {
		fmt.Fprintf(w, "Blocked: %s\n", t.BlockedReason)
	}
	if t.Description != "" {
		fmt.Fprintf(w, "\n%s\n", strings.TrimSpace(t.Description))
	}
	fmt.Fprintf(w, "\nGate (%s): ", t.Gate.Kind)
	switch {
	case t.Gate.BlockedReason != "":
		fmt.Fprintf(w, "blocked — %s\n", t.Gate.BlockedReason)
	case t.Gate.HumanApproved:
		fmt.Fprintln(w, "approved")
	case t.Gate.Kind == "auto":
		fmt.Fprintln(w, "waits for its checks")
	default:
		fmt.Fprintln(w, "waits for a person's approval (gator task approve)")
	}
	for _, c := range t.Gate.Checks {
		line := fmt.Sprintf("  %-7s %s", c.Status, c.Name)
		if c.Detail != "" {
			line += " — " + c.Detail
		}
		fmt.Fprintln(w, line)
	}
	if len(jobs) > 0 {
		fmt.Fprintln(w, "\nJobs:")
		tw := table(env)
		for _, j := range lastN(jobs, 5) {
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n", j.ID, j.Phase, j.Status, orDefault(j.Placement, "-"), j.CreatedAt.Local().Format(time.DateTime))
		}
		_ = tw.Flush()
	}
	return nil
}

func lastN[T any](s []T, n int) []T {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

var taskKinds = []string{"feature", "bug", "chore", "incident", "campaign"}

func cmdTaskNew(ctx context.Context, env *Env, args []string) error {
	fs, g := newFlags("task new")
	project := fs.String("project", "", "project slug; the current project when omitted")
	kind := fs.String("kind", "feature", "feature | bug | chore | incident | campaign")
	description := fs.String("description", "", "what you want, in your words; the agent reads it")
	urgency := fs.Int("urgency", 0, "1 (most urgent) to 4")
	positional, err := parse(fs, args)
	if err != nil {
		return err
	}
	title := strings.TrimSpace(strings.Join(positional, " "))
	if title == "" {
		return coded(exitUserErr, "give the task a title: gator task new \"Fix the login redirect\"")
	}
	if !contains(taskKinds, *kind) {
		return coded(exitUserErr, "--kind must be one of %s", strings.Join(taskKinds, ", "))
	}
	if *urgency < 0 || *urgency > 4 {
		return coded(exitUserErr, "--urgency is 1 to 4")
	}
	s, err := open(env, g)
	if err != nil {
		return err
	}
	ref := s.projectRef(*project)
	if ref == "" {
		return coded(exitUserErr, "which project? pass --project SLUG or choose one with gator use SLUG")
	}
	p, err := resolveProject(ctx, s.client, ref)
	if err != nil {
		return err
	}
	t, err := s.client.CreateTask(ctx, p.ID, NewTask{Kind: *kind, Title: title, Description: *description, Urgency: *urgency})
	if err != nil {
		return err
	}
	if g.json {
		return printJSON(env, t)
	}
	fmt.Fprintf(env.Stdout, "Created %s %s in %s, phase %s.\n", t.Kind, t.ID, p.Slug, t.Phase)
	fmt.Fprintf(env.Stdout, "Run it with gator task run %s\n", t.ID)
	return nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func cmdTaskAdvance(ctx context.Context, env *Env, args []string) error {
	fs, g := newFlags("task advance")
	reason := fs.String("reason", "", "why, for the task's history")
	return moveTask(env, fs, g, args, func(c *Client, id string) (Task, error) { return c.Advance(ctx, id, *reason) })
}

func cmdTaskApprove(ctx context.Context, env *Env, args []string) error {
	fs, g := newFlags("task approve")
	return moveTask(env, fs, g, args, func(c *Client, id string) (Task, error) { return c.Approve(ctx, id) })
}

// moveTask is advance and approve: one task id in, the task in its new phase out.
func moveTask(env *Env, fs *flag.FlagSet, g *globals, args []string, move func(*Client, string) (Task, error)) error {
	positional, err := parse(fs, args)
	if err != nil {
		return err
	}
	id, err := oneArg(positional, "task id")
	if err != nil {
		return err
	}
	s, err := open(env, g)
	if err != nil {
		return err
	}
	t, err := move(s.client, id)
	if err != nil {
		return err
	}
	if g.json {
		return printJSON(env, t)
	}
	if t.ClosedAt != nil {
		fmt.Fprintf(env.Stdout, "Task %s is done (%s).\n", t.ID, t.Phase)
	} else {
		fmt.Fprintf(env.Stdout, "Task %s is now in %s.\n", t.ID, t.Phase)
	}
	return nil
}

func cmdTaskRun(ctx context.Context, env *Env, args []string) error {
	fs, g := newFlags("task run")
	cloud := fs.Bool("cloud", false, "run on Gator's cloud runners instead of a runner of yours")
	instruction := fs.String("instruction", "", "a word for the agent on top of the phase's own")
	backend := fs.String("backend", "", "claude | codex | pi; the project's policy when omitted")
	follow := fs.Bool("f", false, "follow the job's log until it ends")
	fs.BoolVar(follow, "follow", false, "same as -f")
	positional, err := parse(fs, args)
	if err != nil {
		return err
	}
	id, err := oneArg(positional, "task id")
	if err != nil {
		return err
	}
	s, err := open(env, g)
	if err != nil {
		return err
	}
	in := NewJob{Instruction: *instruction, Backend: *backend}
	if *cloud {
		in.Placement = "cloud"
	}
	job, err := s.client.CreateJob(ctx, id, in)
	var ae *APIError
	if *cloud && errors.As(err, &ae) && ae.Status == http.StatusBadRequest && strings.Contains(ae.Message, "placement") {
		return coded(exitUserErr, "%s does not know about cloud runs yet (it refused the placement field: %s); update the server, or run without --cloud", s.server, ae.Message)
	}
	if err != nil {
		return err
	}
	if g.json && !*follow {
		return printJSON(env, job)
	}
	if !g.json {
		where := orDefault(job.Placement, map[bool]string{true: "cloud", false: "local"}[*cloud])
		fmt.Fprintf(env.Stdout, "Queued job %s for %s (%s, %s).\n", job.ID, job.Phase, job.Role, where)
		if !*follow {
			fmt.Fprintf(env.Stdout, "Watch it with gator task logs %s -f\n", id)
			return nil
		}
	}
	return streamJob(ctx, env, s, job, g.json, true)
}
