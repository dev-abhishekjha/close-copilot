package gates

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Risk classes.
const (
	RiskStandard      = "standard"
	RiskDataSensitive = "data-sensitive"
	RiskRegulated     = "regulated"
)

// Risks lists the valid risk classes.
var Risks = []string{RiskStandard, RiskDataSensitive, RiskRegulated}

// Owners lists who may own a ticket: the worker subagents in .claude/agents
// (the security reviewer only reports, so it owns nothing), the human owner
// and the orchestrator.
var Owners = []string{
	"implementer", "integration-engineer", "domain-data-engineer",
	"llm-engineer", "eval-engineer", "human", "orchestrator",
}

// Budget bounds a ticket's build.
type Budget struct {
	MaxAttempts    int `yaml:"max_attempts" json:"max_attempts"`
	MaxWallMinutes int `yaml:"max_wall_minutes" json:"max_wall_minutes"`
}

// Spec is the YAML front matter of specs/<ID>.md (see specs/_template.md).
// Phase and Budget are pointers so that a missing value is distinguishable
// from phase 0.
type Spec struct {
	ID           string   `yaml:"id"`
	Title        string   `yaml:"title"`
	Phase        *int     `yaml:"phase"`
	OwnerRole    string   `yaml:"owner_role"`
	Risk         string   `yaml:"risk"`
	ApprovedBy   string   `yaml:"approved_by"`
	DependsOn    []string `yaml:"depends_on"`
	NeedsERPNext bool     `yaml:"needs_erpnext"`
	Files        []string `yaml:"files"`
	Consumes     []string `yaml:"consumes"`
	Produces     []string `yaml:"produces"`
	Acceptance   []string `yaml:"acceptance"`
	Gates        []string `yaml:"gates"`
	Budget       *Budget  `yaml:"budget"`
}

// MaxAttempts returns the budget's attempt limit, or 0 when unset.
func (s Spec) MaxAttempts() int {
	if s.Budget == nil {
		return 0
	}
	return s.Budget.MaxAttempts
}

// FrontMatter returns the text between the first two "---" lines. The first
// line of the file must be "---".
func FrontMatter(data []byte) ([]byte, error) {
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	if strings.TrimRight(lines[0], " \t") != "---" {
		return nil, errors.New("no front matter: the first line must be ---")
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], " \t") == "---" {
			return []byte(strings.Join(lines[1:i], "\n")), nil
		}
	}
	return nil, errors.New("front matter is not closed by a second --- line")
}

// ParseSpec parses a spec file's front matter. Unknown fields are an error,
// so a misspelt key such as "need_erpnext" cannot pass silently.
func ParseSpec(data []byte) (Spec, error) {
	fm, err := FrontMatter(data)
	if err != nil {
		return Spec{}, err
	}
	var s Spec
	if err := decodeStrict(fm, &s); err != nil {
		return Spec{}, fmt.Errorf("front matter: %w", err)
	}
	return s, nil
}

// LoadSpec reads and parses a spec file.
func LoadSpec(path string) (Spec, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the path is the operator's command-line argument
	if err != nil {
		return Spec{}, fmt.Errorf("read spec: %w", err)
	}
	s, err := ParseSpec(data)
	if err != nil {
		return Spec{}, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

func decodeStrict(data []byte, out any) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("empty document")
		}
		return err
	}
	return nil
}

func validRisk(r string) bool  { return slices.Contains(Risks, r) }
func validOwner(o string) bool { return slices.Contains(Owners, o) }
