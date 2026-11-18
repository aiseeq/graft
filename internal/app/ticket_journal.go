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

func (a *App) ticketJournal(ctx context.Context, repo *gitx.Repo, msg, kind string) {
	root, skipped, err := ticketJournalRoot(repo)
	if err != nil {
		a.warn("ticket journal was not written: finding main checkout: %v", err)
		return
	}
	if skipped {
		fmt.Fprintln(a.Stderr, "graft: ticket journal skipped: separate git dir")
		return
	}
	tasks := filepath.Join(root, "docs", "tasks")
	st, err := os.Stat(tasks)
	if os.IsNotExist(err) || err == nil && !st.IsDir() {
		return
	}
	if err != nil {
		a.warn("ticket journal was not written: checking docs/tasks: %v", err)
		return
	}
	tk, err := journalTKPath()
	if err != nil {
		a.warn("ticket journal was not written: %v", err)
		return
	}
	if tk == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, ticketJournalTimeout)
	defer cancel()
	env, err := journalEnv()
	if err != nil {
		a.warn("ticket journal was not written: %v", err)
		return
	}
	id, err := a.journalTicketID(ctx, repo, root, msg, tk, env)
	if err != nil {
		a.warn("ticket journal was not written: %v", err)
		return
	}
	if id == "" {
		return
	}
	note := fmt.Sprintf("%s graft %s", time.Now().Format("15:04 MST"), kind)
	if _, err := runTicketCommand(ctx, root, env, tk, "note", id, note, "--by", "graft"); err != nil {
		a.warn("ticket journal was not written: tk note: %v", err)
	}
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

func (a *App) journalTicketID(ctx context.Context, repo *gitx.Repo, root, msg, tk string, env []string) (string, error) {
	out, err := runTicketCommand(ctx, root, env, tk, "ls", "--json", "--root", root)
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("tk ls timed out")
		} else {
			return "", fmt.Errorf("tk ls: %w", err)
		}
	}
	tickets, err := parseJournalTickets(out)
	if err != nil {
		return "", fmt.Errorf("tk ls returned invalid JSON: %w", err)
	}
	branch, branchErr := repo.Branch()
	if branchErr != nil {
		return "", fmt.Errorf("reading branch: %w", branchErr)
	}
	return chooseJournalTicket(tickets, msg, branch, root), nil
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

func chooseJournalTicket(tickets []journalTicket, msg, branch, root string) string {
	text := msg + "\n" + branch
	for _, ticket := range tickets {
		if ticketInRootTasks(ticket.Path, root) && ticket.Slug != "" && !ticketClosed(ticket.Status) && containsWholeSlug(text, ticket.Slug) {
			return ticket.Slug
		}
	}
	var active []journalTicket
	for _, ticket := range tickets {
		if ticketInRootTasks(ticket.Path, root) && ticketInProgress(ticket.Status) {
			active = append(active, ticket)
		}
	}
	if len(active) == 1 {
		return active[0].Slug
	}
	return ""
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

func ticketInProgress(status string) bool {
	s := strings.ToLower(strings.TrimSpace(status))
	return s == "в работе" || s == "in progress" || s == "in_progress"
}

func ticketClosed(status string) bool {
	s := strings.ToLower(strings.TrimSpace(status))
	return s == "готово" || s == "закрыт" || s == "закрыта" || s == "закрыто" || s == "closed" || s == "done" || s == "отклонён" || s == "отклонена" || s == "отклонено" || s == "rejected"
}
