package main

import (
	_ "embed"
	"fmt"
	"strings"

	"github.com/aiseeq/graft/v2/internal/app"
)

//go:embed docs/agents.md
var agentGuide string

func guideCmd(a *app.App, args []string) error {
	if len(args) == 0 {
		_, err := fmt.Fprint(a.Stdout, agentGuide)
		return err
	}
	sections := strings.Split(agentGuide, "\n## ")
	var names []string
	for _, section := range sections[1:] {
		name, body, _ := strings.Cut(section, "\n")
		names = append(names, name)
		first, _, _ := strings.Cut(name, " ")
		if len(args) == 1 && (strings.EqualFold(args[0], name) || strings.EqualFold(args[0], first)) {
			_, err := fmt.Fprintf(a.Stdout, "## %s\n%s", name, body)
			return err
		}
	}
	return fmt.Errorf("%w: guide section must be one of: %s", errUsage, strings.Join(names, ", "))
}
