package server_test

// The gap table must not claim a gap that has been closed.
//
// This exists because the table did exactly that. At one point it contained two rows that
// contradicted each other: one said the ACL API had "没有 UI … 前端未接入", while another said the
// ACL screen existed and described it. Both had been written honestly; the first simply outlived
// the fact. A status document whose "not done" column is stale is actively harmful — it sends the
// next person to rebuild something that already works, or to distrust the rest of the list.
//
// The check is a small, explicit assertion list: each entry claims that a named artefact does not
// exist, and the test fails if it does. That is narrower than "audit the whole document", which is
// not mechanisable — but it covers the specific shapes the table kept getting wrong.
//
// A row that is genuinely still a gap stays in the file and keeps passing. When someone closes the
// gap, the claim fails and they are told to update the row. That is the intended direction: the
// test pulls the documentation forward rather than trusting anyone to remember.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// absenceClaim is a statement of the form "X is not in the tree", which the test falsifies.
type absenceClaim struct {
	// because is the phrase the documentation used, quoted so the failure message points at it.
	because string
	// artefact is a path that must not exist.
	artefact string
	// row names the gap row that makes the claim, so the message can say what to edit.
	row string
}

// TestGapTableDoesNotClaimClosedGapsAreOpen.
func TestGapTableDoesNotClaimClosedGapsAreOpen(t *testing.T) {
	root := repoRoot(t)

	record, err := os.ReadFile(filepath.Join(root, "docs", "0011-contract-implementation-record.md"))
	if err != nil {
		t.Fatalf("read the implementation record: %v", err)
	}
	doc := string(record)

	claims := []absenceClaim{
		{
			because:  "前端未接入",
			artefact: "web/src/components/FolderAccess.tsx",
			row:      "§4.1 ACL 的界面",
		},
		{
			because:  "没有 UI",
			artefact: "web/src/views/Admin.tsx",
			row:      "ACL / 账号审批 UI",
		},
		{
			because:  "没有随附的 Grafana 面板或告警规则",
			artefact: "deploy/monitoring/alerts.yml",
			row:      "§11.1 指标的图表",
		},
		{
			because:  "restic 在当前环境没有安装",
			artefact: "deploy/backup/blob-backup.sh",
			row:      "INV-08 restic 恢复",
		},
	}

	// Only table rows are checked, not the whole document.
	//
	// The narrative legitimately quotes a stale row when explaining that it was fixed ("the table
	// said 前端未接入"), and matching that prose would make the guard fire on its own changelog —
	// which is exactly what happened the first time this test ran against the round's notes. The
	// claim lives in the table; the check reads the table.
	var tableRows []string
	for _, line := range strings.Split(doc, "\n") {
		if strings.HasPrefix(line, "|") {
			tableRows = append(tableRows, line)
		}
	}
	if len(tableRows) == 0 {
		t.Fatal("found no table rows; this check would pass vacuously")
	}
	table := strings.Join(tableRows, "\n")

	for _, c := range claims {
		target := filepath.Join(root, filepath.FromSlash(c.artefact))
		_, statErr := os.Stat(target)
		exists := statErr == nil
		if !exists {
			continue
		}
		// The artefact is there. If a table row still says it is not, that row is stale.
		if strings.Contains(table, c.because) {
			t.Fatalf("the gap table still says %q (row: %s), but %s exists.\n\n"+
				"Update that row: a status document whose \"not done\" column is stale sends the "+
				"next person to rebuild something that already works.",
				c.because, c.row, c.artefact)
		}
	}
}

// TestGapTableRowsAreWellFormed checks the table's shape rather than its content.
//
// Two columns, no row with a missing impact cell, and no duplicated row name. The table is edited
// by splicing lines, and a botched splice has silently produced a malformed row before.
func TestGapTableRowsAreWellFormed(t *testing.T) {
	root := repoRoot(t)
	record, err := os.ReadFile(filepath.Join(root, "docs", "0011-contract-implementation-record.md"))
	if err != nil {
		t.Fatalf("read the implementation record: %v", err)
	}
	lines := strings.Split(string(record), "\n")

	header := -1
	for i, line := range lines {
		if regexp.MustCompile(`^\|\s*需求\s*\|\s*现状\s*\|\s*影响\s*\|`).MatchString(line) {
			header = i
			break
		}
	}
	if header < 0 {
		t.Fatal("could not find the gap table header (| 需求 | 现状 | 影响 |)")
	}
	if header+1 >= len(lines) || !strings.Contains(lines[header+1], "---") {
		t.Fatal("the gap table header is not followed by a separator row")
	}

	seen := map[string]int{}
	rows := 0
	for i := header + 2; i < len(lines); i++ {
		line := lines[i]
		if !strings.HasPrefix(line, "|") {
			break
		}
		cells := strings.Split(line, "|")
		// A well-formed row splits into: "", col1, col2, col3, "" — five parts.
		if len(cells) != 5 {
			t.Fatalf("line %d has %d cells; the table has three columns, so a row must have "+
				"exactly five pipe-separated parts (including the empty ends):\n%s",
				i+1, len(cells), line)
		}
		name := strings.TrimSpace(cells[1])
		if name == "" {
			t.Fatalf("line %d has an empty requirement cell:\n%s", i+1, line)
		}
		if strings.TrimSpace(cells[2]) == "" {
			t.Fatalf("line %d (%s) has an empty status cell", i+1, name)
		}
		if strings.TrimSpace(cells[3]) == "" {
			t.Fatalf("line %d (%s) has an empty impact cell", i+1, name)
		}
		if prev, dup := seen[name]; dup {
			t.Fatalf("the requirement %q appears twice (lines %d and %d). One of them is a "+
				"stale copy — that is how the ACL row ended up contradicting itself.",
				name, prev, i+1)
		}
		seen[name] = i + 1
		rows++
	}
	if rows == 0 {
		t.Fatal("the gap table has no rows; this check would pass vacuously")
	}
}
