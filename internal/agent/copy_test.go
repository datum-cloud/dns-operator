// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"strings"
	"testing"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	agentdocs "go.miloapis.com/dns-operator/docs/agent"
	sharedutil "go.miloapis.com/dns-operator/internal/dns/util"
)

// The copy in this package is read by a customer, not by whoever wrote the
// operator. These tests pin both halves: the words are the customer's, and
// the facts they need in order to escalate survive the translation.

type bannedTerm struct {
	pattern *regexp.Regexp
	why     string
}

func banned(pattern, why string) bannedTerm {
	return bannedTerm{pattern: regexp.MustCompile(`(?i)` + pattern), why: why}
}

// internalVocabulary is the denylist: words that describe how the operator
// and its downstream infrastructure are built, which a customer reading a
// diagnosis has no way to act on.
func internalVocabulary() []bannedTerm {
	return []bannedTerm{
		banned(`\bshadow\s?zones?\b`,
			"the downstream replica of a zone on the shared DNS infrastructure cluster. A customer "+
				"has one zone and one name for it."),
		banned(`\bdownstream\b`,
			"describes the shared DNS infrastructure cluster versus the customer's own project. Say "+
				"the DNS backend, or say Datum."),
		banned(`\breplicators?\b`,
			"the internal controller that copies records to the shared infrastructure cluster. Say "+
				"Datum."),
		banned(`\breconcil`,
			"controller-loop vocabulary. It describes how Datum works, never what happened to the "+
				"customer's zone or record."),
		banned(`\baccounting\s?configmaps?\b`,
			"an internal bookkeeping object with no customer-visible counterpart."),
		banned(`\bpowerdns\b`,
			"the name of the DNS provider Datum runs behind the scenes. Say the DNS backend."),
		banned(`\bcontrol planes?\b`,
			"how Datum is built, not something a customer with a stuck zone reasons about."),
	}
}

// apiIdentifiers are the strings that may appear in copy verbatim: reason
// codes, condition types, skill and pattern names. A customer quotes them
// when escalating, so they are removed before the prose is scanned.
func apiIdentifiers() []string {
	seen := map[string]struct{}{
		PatternOrphanedBackendRecord: {},
		SkillZoneNotResolving:        {},
		SkillRecordNotProgrammed:     {},
		SkillConflictingRecord:       {},
		SkillRecordNotOwner:          {},
		SkillDelegationCheck:         {},
		SkillDomainVerification:      {},
		SkillManagedRecord:           {},
	}
	for _, info := range AllReasons() {
		seen[info.Reason] = struct{}{}
		for _, ct := range info.ConditionTypes {
			seen[ct] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if len(out[j]) > len(out[i]) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func prose(s string, extra ...string) string {
	for _, id := range append(apiIdentifiers(), extra...) {
		if id != "" {
			s = strings.ReplaceAll(s, id, " ")
		}
	}
	return s
}

func checkCopy(t *testing.T, where, text string, terms []bannedTerm, extra ...string) {
	t.Helper()
	p := prose(text, extra...)
	for _, term := range terms {
		if m := term.pattern.FindString(p); m != "" {
			t.Errorf("%s uses %q, which a customer has no way to read.\n  why: %s\n  in: %s",
				where, m, term.why, strings.TrimSpace(text))
		}
	}
}

// TestCatalogCopyUsesNoInternalVocabulary covers the table the assistant
// reads out almost verbatim.
func TestCatalogCopyUsesNoInternalVocabulary(t *testing.T) {
	terms := internalVocabulary()
	for _, info := range AllReasons() {
		checkCopy(t, info.Reason+".Explanation", info.Explanation, terms)
		checkCopy(t, info.Reason+".Remediation", info.Remediation, terms)
	}
}

// TestDiagnosisCopyUsesNoInternalVocabulary covers the sentences the walk
// assembles at read time, including the orphaned-conflict rewrite, which the
// catalog test cannot see.
func TestDiagnosisCopyUsesNoInternalVocabulary(t *testing.T) {
	terms := internalVocabulary()
	for name, d := range diagnosisFixtures() {
		names := []string{d.Zone, d.OwnerName}
		for _, c := range d.ContributingConditions {
			names = append(names, c.Object)
		}
		checkCopy(t, name+".Summary", d.Summary, terms, names...)
		for _, c := range d.ContributingConditions {
			checkCopy(t, name+"."+c.Reason+".Explanation", c.Explanation, terms, names...)
			checkCopy(t, name+"."+c.Reason+".Remediation", c.Remediation, terms, names...)
		}
		for i, s := range d.NextSteps {
			checkCopy(t, fmt.Sprintf("%s.NextSteps[%d]", name, i), s, terms, names...)
		}
	}
}

// TestPublishedDocsUseNoInternalVocabulary covers the knowledge document and
// the skills. They may say "controller" or "condition" — they address the
// assistant — but a skill that says "shadow zone" produces an answer that
// says "shadow zone".
func TestPublishedDocsUseNoInternalVocabulary(t *testing.T) {
	terms := internalVocabulary()
	entries, err := fs.ReadDir(agentdocs.FS, agentdocs.SkillsDir)
	if err != nil {
		t.Fatalf("reading %s: %v", agentdocs.SkillsDir, err)
	}
	files := make([]string, 0, 1+len(entries))
	files = append(files, agentdocs.KnowledgeFile)
	for _, e := range entries {
		files = append(files, path.Join(agentdocs.SkillsDir, e.Name()))
	}
	if len(files) < 4 {
		t.Fatalf("found %d published documents, want the knowledge document and at least three skills", len(files))
	}
	for _, name := range files {
		b, err := fs.ReadFile(agentdocs.FS, name)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			checkCopy(t, fmt.Sprintf("%s:%d", name, i+1), line, terms)
		}
	}
}

// TestPlainLanguageKeepsTheEvidence is the counterweight: every identifier a
// customer needs in order to escalate must survive the rewrite.
func TestPlainLanguageKeepsTheEvidence(t *testing.T) {
	stale := stagingNow.Add(-time.Hour)
	z := zone("example.com",
		cond(sharedutil.CondAccepted, "True", sharedutil.ReasonAccepted, "ok"),
		condAt(sharedutil.CondProgrammed, "False", sharedutil.ReasonPending, "waiting", stale))

	d := DiagnoseZoneAt(stagingNow, z)
	for _, want := range []string{
		"example.com",
		sharedutil.ReasonPending,
		sharedutil.CondProgrammed,
	} {
		if !strings.Contains(d.Summary, want) {
			t.Errorf("Summary = %q, want it to keep the evidence %q", d.Summary, want)
		}
	}
	if d.RootCause.LastTransitionTime == "" {
		t.Error("LastTransitionTime is empty; the exact timestamp is evidence and must survive")
	}
}

// diagnosisFixtures exercises every branch that assembles prose at read time.
func diagnosisFixtures() map[string]Diagnosis {
	fresh := stagingNow.Add(-2 * time.Minute)
	stale := stagingNow.Add(-2 * time.Hour)

	out := map[string]Diagnosis{}

	out["zone-healthy"] = DiagnoseZoneAt(stagingNow, zone("example.com",
		cond(sharedutil.CondAccepted, "True", sharedutil.ReasonAccepted, "ok"),
		cond(sharedutil.CondProgrammed, "True", sharedutil.ReasonProgrammed, "ok")))

	out["zone-rejected"] = DiagnoseZoneAt(stagingNow, zone("example.com",
		cond(sharedutil.CondAccepted, "False", sharedutil.ReasonInvalidDNSRecordSet, "bad zone")))

	out["zone-pending"] = DiagnoseZoneAt(stagingNow, zone("example.com",
		cond(sharedutil.CondAccepted, "True", sharedutil.ReasonAccepted, "ok"),
		condAt(sharedutil.CondProgrammed, "False", sharedutil.ReasonPending, "waiting", fresh)))

	out["zone-stalled"] = DiagnoseZoneAt(stagingNow, zone("example.com",
		cond(sharedutil.CondAccepted, "True", sharedutil.ReasonAccepted, "ok"),
		condAt(sharedutil.CondProgrammed, "False", sharedutil.ReasonPending, "waiting", stale)))

	out["zone-platform"] = DiagnoseZoneAt(stagingNow, zone("example.com",
		cond(sharedutil.CondAccepted, "False", sharedutil.ReasonDNSZoneInUse, "already claimed")))

	notOwner := recordSet("www", []dnsv1alpha1.RecordEntry{{Name: "www"}},
		ownerStatus("www.example.com.", cond(sharedutil.CondProgrammed, "False", sharedutil.ReasonNotOwner, "taken")))
	out["record-not-owner"] = DiagnoseRecordAt(stagingNow, zone("example.com"),
		[]dnsv1alpha1.DNSRecordSet{*notOwner}, notOwner, "www")

	conflictTarget := recordSet("www-cname", []dnsv1alpha1.RecordEntry{{Name: "www"}},
		ownerStatus("www.example.com.", cond(sharedutil.CondProgrammed, "False", sharedutil.ReasonConflict,
			"A conflicting record already exists for this name. Remove the existing record and try again.")))
	out["record-conflict-orphaned"] = DiagnoseRecordAt(stagingNow, zone("example.com"),
		[]dnsv1alpha1.DNSRecordSet{*conflictTarget}, conflictTarget, "www")

	conflictSibling := recordSet("www-a", []dnsv1alpha1.RecordEntry{{Name: "www"}})
	out["record-conflict-genuine"] = DiagnoseRecordAt(stagingNow, zone("example.com"),
		[]dnsv1alpha1.DNSRecordSet{*conflictTarget, *conflictSibling}, conflictTarget, "www")

	pdnsInvalid := recordSet("txt", []dnsv1alpha1.RecordEntry{{Name: "txt"}},
		ownerStatus("txt.example.com.", cond(sharedutil.CondProgrammed, "False", sharedutil.ReasonPDNSError,
			"The record content contains an invalid character. TXT records containing semicolons or special characters must be properly quoted.")))
	out["record-pdns-invalid"] = DiagnoseRecordAt(stagingNow, zone("example.com"),
		[]dnsv1alpha1.DNSRecordSet{*pdnsInvalid}, pdnsInvalid, "txt")

	mystery := recordSet("mystery", []dnsv1alpha1.RecordEntry{{Name: "mystery"}},
		ownerStatus("mystery.example.com.", cond(sharedutil.CondProgrammed, "False", "SomeBrandNewReason", "Something new.")))
	out["record-uncatalogued"] = DiagnoseRecordAt(stagingNow, zone("example.com"),
		[]dnsv1alpha1.DNSRecordSet{*mystery}, mystery, "mystery")

	return out
}
