package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aiseeq/graft/v2/internal/execx"
	"github.com/aiseeq/graft/v2/internal/gitx"
)

const ticketJournalTimeout = 5 * time.Second

type journalTicket struct {
	Slug     string `json:"slug"`
	Status   string `json:"status"`
	Assignee any    `json:"assignee"`
	Path     string `json:"path"`
}

type ticketJournalTarget struct {
	id, tkID, path string
	root           string
	tk             string
	env            []string
	inTree         bool
}

func (a *App) ticketJournal(ctx context.Context, repo *gitx.Repo, msg, kind string) {
	a.writeTicketJournal(ctx, repo, msg, kind, true)
}

func (a *App) ticketJournalForMessage(ctx context.Context, repo *gitx.Repo, msg, kind string) {
	a.writeTicketJournal(ctx, repo, msg, kind, false)
}

func (a *App) writeTicketJournal(ctx context.Context, repo *gitx.Repo, msg, kind string, includeBranch bool) {
	ctx, cancel := context.WithTimeout(ctx, ticketJournalTimeout)
	defer cancel()
	target, skipped, err := a.findTicketJournalTarget(ctx, repo, msg, includeBranch)
	if err != nil {
		a.warn("ticket journal was not written: finding main checkout: %v", err)
		return
	}
	if skipped {
		fmt.Fprintln(a.Stderr, "graft: ticket journal skipped: separate git dir")
		return
	}
	if target == nil {
		return
	}
	if target.inTree {
		if strings.HasPrefix(kind, "commit ") && filepath.Clean(repo.Root) == filepath.Clean(target.root) {
			return
		}
		fmt.Fprintf(a.Stderr, "graft: ticket %s: not noted (docs/tasks is tracked): %s\n", target.id, kind)
		return
	}
	note := fmt.Sprintf("%s graft %s", time.Now().Format("15:04 MST"), kind)
	if _, err := runTicketCommand(ctx, target.root, target.env, target.tk, "note", target.tkID, note, "--by", "graft"); err != nil {
		a.warn("ticket journal was not written: tk note: %v", err)
	}
}

// prepareCommitTicketJournal writes a tracked ticket note into the index so it
// becomes part of the commit. The returned rollback restores both file bytes
// and the index entry if git commit fails.
func (a *App) prepareCommitTicketJournal(ctx context.Context, repo *gitx.Repo, plan commitPlan) (func() error, error) {
	root, skipped, err := ticketJournalRoot(repo)
	if err != nil || skipped {
		return nil, err
	}
	if filepath.Clean(repo.Root) != filepath.Clean(root) {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, ticketJournalTimeout)
	defer cancel()
	target, _, err := a.findTicketJournalTarget(ctx, repo, plan.message, true)
	if err != nil || target == nil || !target.inTree || target.id == "" || plan.release {
		return nil, err
	}
	first, _, _ := strings.Cut(plan.message, "\n")
	line := fmt.Sprintf("graft commit on %s: %s", plan.branch, strings.TrimSpace(first))
	if plan.head.sha != "" {
		line = fmt.Sprintf("graft commit after %s on %s: %s", gitx.Short(plan.head.sha), plan.branch, strings.TrimSpace(first))
	}
	file, err := os.ReadFile(target.path)
	if err != nil {
		return nil, fmt.Errorf("reading ticket before note: %w", err)
	}
	info, err := os.Stat(target.path)
	if err != nil {
		return nil, fmt.Errorf("checking ticket before note: %w", err)
	}
	index, err := saveIndex(repo, plan.head)
	if err != nil {
		return nil, err
	}
	if _, err := runTicketCommand(ctx, target.root, target.env, target.tk, "note", target.tkID, line, "--by", "graft"); err != nil {
		return nil, errors.Join(fmt.Errorf("tk note: %w", err), restoreJournalFile(target.path, file, info.Mode()))
	}
	if _, err := repo.Git("add", "--", target.path); err != nil {
		return nil, errors.Join(err, restoreJournalFile(target.path, file, info.Mode()), restoreIndex(index))
	}
	rollback := func() error {
		return errors.Join(restoreJournalFile(target.path, file, info.Mode()), restoreIndex(index))
	}
	return rollback, nil
}

func restoreJournalFile(path string, contents []byte, mode os.FileMode) error {
	return os.WriteFile(path, contents, mode.Perm())
}

func (a *App) findTicketJournalTarget(ctx context.Context, repo *gitx.Repo, msg string, includeBranch bool) (*ticketJournalTarget, bool, error) {
	root, skipped, tk, env, tickets, branch, err := loadJournalCandidates(ctx, repo, includeBranch)
	if err != nil || skipped || root == "" {
		return nil, skipped, err
	}
	selected := chooseJournalTicketNamed(tickets, msg, branch, root)
	if selected == nil {
		return nil, false, nil
	}
	target, err := resolveJournalTicket(repo, root, tk, env, tickets, selected)
	return target, false, err
}

func loadJournalCandidates(ctx context.Context, repo *gitx.Repo, includeBranch bool) (string, bool, string, []string, []journalTicket, string, error) {
	root, skipped, err := ticketJournalRoot(repo)
	if err != nil || skipped {
		return "", skipped, "", nil, nil, "", err
	}
	tasks := filepath.Join(root, "docs", "tasks")
	st, err := os.Stat(tasks)
	if os.IsNotExist(err) || err == nil && !st.IsDir() {
		return "", false, "", nil, nil, "", nil
	}
	if err != nil {
		return "", false, "", nil, nil, "", fmt.Errorf("checking docs/tasks: %w", err)
	}
	tk, err := journalTKPath()
	if err != nil || tk == "" {
		return "", false, "", nil, nil, "", err
	}
	env, err := journalEnv()
	if err != nil {
		return "", false, "", nil, nil, "", err
	}
	out, err := runTicketCommand(ctx, root, env, tk, "ls", "--json", "--root", root)
	if err != nil {
		if ctx.Err() != nil {
			return "", false, "", nil, nil, "", fmt.Errorf("tk ls timed out")
		}
		return "", false, "", nil, nil, "", fmt.Errorf("tk ls: %w", err)
	}
	tickets, err := parseJournalTickets(out)
	if err != nil {
		return "", false, "", nil, nil, "", fmt.Errorf("tk ls returned invalid JSON: %w", err)
	}
	branch := ""
	if includeBranch {
		branch, err = repo.Branch()
		if err != nil {
			return "", false, "", nil, nil, "", fmt.Errorf("reading branch: %w", err)
		}
	}
	return root, false, tk, env, tickets, branch, nil
}

type journalTicketSelection struct {
	slug, tkID string
}

func resolveJournalTicket(repo *gitx.Repo, root, tk string, env []string, tickets []journalTicket, selected *journalTicketSelection) (*ticketJournalTarget, error) {
	var ticket *journalTicket
	for i := range tickets {
		if tickets[i].Slug == selected.slug && ticketInRootTasks(tickets[i].Path, root) {
			ticket = &tickets[i]
			break
		}
	}
	if ticket == nil {
		return nil, fmt.Errorf("selected ticket %q is missing from tk ls output", selected.slug)
	}
	path := ticket.Path
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	resolved, err := resolveTicketPath(path)
	if err != nil {
		return nil, err
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolving repository root: %w", err)
	}
	within := resolved == resolvedRoot || strings.HasPrefix(resolved, resolvedRoot+string(filepath.Separator))
	ignored, err := journalPathIgnored(repo, root, path, within)
	if err != nil {
		return nil, err
	}
	return &ticketJournalTarget{id: selected.slug, tkID: selected.tkID, path: path, root: root, tk: tk, env: env, inTree: within && !ignored}, nil
}

func resolveTicketPath(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if errors.Is(err, os.ErrNotExist) {
		parent, parentErr := filepath.EvalSymlinks(filepath.Dir(path))
		if parentErr == nil {
			return filepath.Join(parent, filepath.Base(path)), nil
		}
	}
	if err != nil {
		return "", fmt.Errorf("resolving ticket path: %w", err)
	}
	return resolved, nil
}

func journalPathIgnored(repo *gitx.Repo, root, path string, within bool) (bool, error) {
	if !within {
		return false, nil
	}
	_, err := repo.Git("-C", root, "check-ignore", "--quiet", "--", path)
	if err == nil {
		return true, nil
	}
	if gitx.ExitCode(err) == 1 {
		return false, nil
	}
	return false, fmt.Errorf("checking whether ticket is ignored: %w", err)
}

func chooseJournalTicketNamed(tickets []journalTicket, msg, branch, root string) *journalTicketSelection {
	text := msg + "\n" + branch
	for _, ticket := range tickets {
		if !ticketInRootTasks(ticket.Path, root) || ticket.Slug == "" || ticketClosed(ticket.Status) {
			continue
		}
		if containsWholeSlug(text, ticket.Slug) {
			return &journalTicketSelection{slug: ticket.Slug, tkID: ticket.Slug}
		}
		filename := filepath.Base(ticket.Path)
		if containsWholeSlug(text, filename) {
			return &journalTicketSelection{slug: ticket.Slug, tkID: filename}
		}
	}
	return nil
}

func ticketJournalRoot(repo *gitx.Repo) (string, bool, error) {
	root := repo.Root
	if filepath.Clean(repo.GitDir) == filepath.Clean(repo.CommonDir) {
		if filepath.Clean(repo.CommonDir) != filepath.Clean(filepath.Join(root, ".git")) {
			return "", true, nil
		}
		return root, false, nil
	}
	var err error
	root, err = repo.MainWorktree()
	if err != nil {
		return "", false, fmt.Errorf("finding main checkout: %w", err)
	}
	separateCheckout, err := hasSeparateGitDirCheckout(repo.CommonDir)
	if err != nil {
		return "", false, fmt.Errorf("checking separate git dir checkout: %w", err)
	}
	if separateCheckout {
		return "", true, nil
	}
	configPath := filepath.Join(root, ".graft.yaml")
	configInfo, err := os.Stat(configPath)
	if errors.Is(err, os.ErrNotExist) || err == nil && !configInfo.Mode().IsRegular() {
		return "", true, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("checking main checkout config %s: %w", configPath, err)
	}
	if filepath.Clean(repo.CommonDir) != filepath.Clean(filepath.Join(root, ".git")) {
		return "", true, nil
	}
	return root, false, nil
}

func hasSeparateGitDirCheckout(commonDir string) (bool, error) {
	metadataParent := filepath.Dir(commonDir)
	searchDir := filepath.Dir(metadataParent)
	entries, err := os.ReadDir(searchDir)
	if err != nil {
		return false, fmt.Errorf("reading %s: %w", searchDir, err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		candidate := filepath.Join(searchDir, entry.Name())
		gitPath := filepath.Join(candidate, ".git")
		gitInfo, err := os.Lstat(gitPath)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("checking %s: %w", gitPath, err)
		}
		if !gitInfo.Mode().IsRegular() {
			continue
		}
		gitFile, err := os.ReadFile(gitPath)
		if err != nil {
			return false, fmt.Errorf("reading %s: %w", gitPath, err)
		}
		line := strings.TrimSpace(string(gitFile))
		target, ok := strings.CutPrefix(line, "gitdir:")
		if !ok {
			continue
		}
		target = strings.TrimSpace(target)
		if !filepath.IsAbs(target) {
			target = filepath.Join(candidate, target)
		}
		if filepath.Clean(target) == filepath.Clean(commonDir) {
			return true, nil
		}
	}
	return false, nil
}

func runTicketCommand(ctx context.Context, dir string, env []string, tk string, args ...string) ([]byte, error) {
	cmd, err := execx.CommandContext(ctx, 0, tk, args...)
	if err != nil {
		return nil, fmt.Errorf("preparing tk command: %w", err)
	}
	cmd.Dir, cmd.Env, cmd.WaitDelay = dir, env, time.Second
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	err = cmd.Run()
	return output.Bytes(), err
}

func ticketInRootTasks(ticketPath, root string) bool {
	if ticketPath == "" {
		return false
	}
	if !filepath.IsAbs(ticketPath) {
		ticketPath = filepath.Join(root, ticketPath)
	}
	want := filepath.Clean(filepath.Join(root, "docs", "tasks"))
	return filepath.Clean(filepath.Dir(ticketPath)) == want
}

func containsWholeSlug(text, slug string) bool {
	for offset := 0; offset < len(text); {
		rel := strings.Index(text[offset:], slug)
		if rel < 0 {
			return false
		}
		start, end := offset+rel, offset+rel+len(slug)
		leftOK := start == 0 || !slugWordRune(runeBefore(text, start))
		rightOK := end == len(text) || !slugWordRune(runeAt(text, end))
		if leftOK && rightOK {
			return true
		}
		offset = start + 1
	}
	return false
}

func runeBefore(text string, byteOffset int) rune {
	r, _ := utf8.DecodeLastRuneInString(text[:byteOffset])
	return r
}

func runeAt(text string, byteOffset int) rune {
	r, _ := utf8.DecodeRuneInString(text[byteOffset:])
	return r
}

func slugWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsNumber(r) || r == '-' || r == '_'
}

func journalTKPath() (string, error) {
	tk, err := exec.LookPath("tk")
	if errors.Is(err, exec.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("finding tk: %w", err)
	}
	return tk, nil
}

func journalEnv() ([]string, error) {
	drop, err := gitx.LocalEnvVars()
	if err != nil {
		return nil, err
	}
	return gitx.Without(os.Environ(), drop), nil
}

func parseJournalTickets(data []byte) ([]journalTicket, error) {
	var shape any
	if err := json.Unmarshal(data, &shape); err != nil {
		return nil, fmt.Errorf("decoding ticket JSON shape: %w", err)
	}
	switch shape.(type) {
	case []any:
		var tickets []journalTicket
		if err := json.Unmarshal(data, &tickets); err != nil {
			return nil, fmt.Errorf("decoding ticket list: %w", err)
		}
		return tickets, nil
	case map[string]any:
		var envelope struct {
			Tickets []journalTicket `json:"tickets"`
		}
		if err := json.Unmarshal(data, &envelope); err != nil {
			return nil, fmt.Errorf("decoding ticket envelope: %w", err)
		}
		return envelope.Tickets, nil
	default:
		return nil, fmt.Errorf("expected array or tickets object")
	}
}

func ticketClosed(status string) bool {
	s := strings.ToLower(strings.TrimSpace(status))
	return s == "готово" || s == "закрыт" || s == "закрыта" || s == "закрыто" || s == "closed" || s == "done" || s == "отклонён" || s == "отклонена" || s == "отклонено" || s == "rejected"
}
