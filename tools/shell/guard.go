// ClawEh
// License: MIT

package shell

import (
	"fmt"
	"regexp"
	"strings"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/logger"
)

var defaultDenyPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\brm\s+-[rf]{1,2}\b`),
	regexp.MustCompile(`\bdel\s+/[fq]\b`),
	regexp.MustCompile(`\brmdir\s+/s\b`),
	// Match disk wiping commands (must be followed by space/args)
	regexp.MustCompile(
		`\b(format|mkfs|diskpart)\b\s`,
	),
	regexp.MustCompile(`\bdd\s+if=`),
	// Block writes to block devices (all common naming schemes).
	regexp.MustCompile(
		`>\s*/dev/(sd[a-z]|hd[a-z]|vd[a-z]|xvd[a-z]|nvme\d|mmcblk\d|loop\d|dm-\d|md\d|sr\d|nbd\d)`,
	),
	regexp.MustCompile(`\b(shutdown|reboot|poweroff)\b`),
	regexp.MustCompile(`:\(\)\s*\{.*\};\s*:`),
	regexp.MustCompile(`\$\([^)]+\)`),
	regexp.MustCompile(`\$\{[^}]+\}`),
	regexp.MustCompile("`[^`]+`"),
	regexp.MustCompile(`\|\s*sh\b`),
	regexp.MustCompile(`\|\s*bash\b`),
	regexp.MustCompile(`;\s*rm\s+-[rf]`),
	regexp.MustCompile(`&&\s*rm\s+-[rf]`),
	regexp.MustCompile(`\|\|\s*rm\s+-[rf]`),
	regexp.MustCompile(`<<\s*EOF`),
	regexp.MustCompile(`\$\(\s*cat\s+`),
	regexp.MustCompile(`\$\(\s*curl\s+`),
	regexp.MustCompile(`\$\(\s*wget\s+`),
	regexp.MustCompile(`\$\(\s*which\s+`),
	regexp.MustCompile(`\bsudo\b`),
	regexp.MustCompile(`\bchmod\s+[0-7]{3,4}\b`),
	regexp.MustCompile(`\bchown\b`),
	regexp.MustCompile(`\bpkill\b`),
	regexp.MustCompile(`\bkillall\b`),
	regexp.MustCompile(`\bkill\b`),
	regexp.MustCompile(`\bcurl\b.*\|\s*(sh|bash)`),
	regexp.MustCompile(`\bwget\b.*\|\s*(sh|bash)`),
	regexp.MustCompile(`\bnpm\s+install\s+-g\b`),
	regexp.MustCompile(`\bpip\s+install\s+--user\b`),
	regexp.MustCompile(`\bapt\s+(install|remove|purge)\b`),
	regexp.MustCompile(`\byum\s+(install|remove)\b`),
	regexp.MustCompile(`\bdnf\s+(install|remove)\b`),
	regexp.MustCompile(`\bdocker\s+run\b`),
	regexp.MustCompile(`\bdocker\s+exec\b`),
	regexp.MustCompile(`\bgit\s+push\b`),
	regexp.MustCompile(`\bgit\s+force\b`),
	regexp.MustCompile(`\bssh\b.*@`),
	regexp.MustCompile(`\beval\b`),
	regexp.MustCompile(`\bsource\s+.*\.sh\b`),
}

// guard screens commands with the deny patterns and the custom allow
// patterns of tools.exec. It does not look at the paths a command names: the
// shell runs with the app account's full access.
type guard struct {
	deny        []*regexp.Regexp
	customAllow []*regexp.Regexp // exempt a command from the deny patterns
}

// newGuard builds the guard of exec (tools.exec).
func newGuard(exec config.ExecConfig) (*guard, error) {
	g := &guard{}
	if exec.EnableDenyPatterns {
		g.deny = append(g.deny, defaultDenyPatterns...)
		for _, pattern := range exec.CustomDenyPatterns {
			re, err := regexp.Compile(pattern)
			if err != nil {
				return nil, fmt.Errorf("invalid custom deny pattern %q: %w", pattern, err)
			}
			g.deny = append(g.deny, re)
		}
	}
	for _, pattern := range exec.CustomAllowPatterns {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid custom allow pattern %q: %w", pattern, err)
		}
		g.customAllow = append(g.customAllow, re)
	}
	return g, nil
}

// logGuardState notes how the guard of exec is set.
func logGuardState(exec config.ExecConfig) {
	switch {
	case !exec.EnableDenyPatterns:
		logger.WarnC("shell", "Deny patterns are disabled - all commands will be allowed")
	case len(exec.CustomDenyPatterns) > 0:
		logger.InfoCF("shell", "Using custom deny patterns", map[string]any{"patterns": exec.CustomDenyPatterns})
	}
}

// check returns why command is refused, or "" to allow it.
func (g *guard) check(command string) string {
	lower := strings.ToLower(strings.TrimSpace(command))
	// Custom allow patterns exempt a command from deny checks.
	for _, pattern := range g.customAllow {
		if pattern.MatchString(lower) {
			return ""
		}
	}
	for _, pattern := range g.deny {
		if pattern.MatchString(lower) {
			return "Command blocked by safety guard (dangerous pattern detected)"
		}
	}
	return ""
}
