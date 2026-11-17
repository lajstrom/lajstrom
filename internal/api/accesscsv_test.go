package api

import (
	"bytes"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
)

// TestAccessReportCSV: one line per binding, token and grant, in the order of the document, the
// flags of the entry and of the item joined by ;, after a header.
func TestAccessReportCSV(t *testing.T) {
	var buf bytes.Buffer
	must(t, NewAccessReport(accessRows(), accessConfig, AccessQuery{Scope: auth.AccessAll, UnusedDays: 90}).WriteCSV(&buf))
	u := func(n int) string { return "user:" + accessUID(n) }
	want := strings.Join([]string{
		"principal,kind,name,item,role,mask,scopes,expiresAt,lastUsedAt,via,flags",
		u(1) + ",local,alice,binding,admin,*/*,,,,,admin;allRepos;adminWithoutEffect",
		u(1) + ",local,alice,token,,,,2026-12-18T10:00:00Z,2026-05-02T10:00:00Z,,admin;allRepos;adminWithoutEffect;unused",
		u(2) + ",oidc,bob,binding,writer,raw/*,,,,group:ops,unused;inactive",
		u(3) + ",service,robot,binding,admin,raw/app,,,,,admin;adminWithoutEffect",
		u(3) + ",service,robot,token,,,read;write,,,,admin;adminWithoutEffect;noExpiry;beyondMaxTTL",
		"group:nobody,group,nobody,binding,reader,raw/app,,,,,",
		"group:ops,group,ops,binding,writer,raw/*,,,,,",
		"cert:spiffe://corp/ci/*,cert,spiffe://corp/ci/*,binding,reader,raw/app,,,,,",
		"k8s:prod/build/runner,k8s,prod/build/runner,binding,writer,raw/*,,,,,",
		"token:12,deploy,node,token,,raw/app,read,,,,noExpiry;beyondMaxTTL",
		"token:13,deploy,fleet,token,,*/*,read,2026-11-28T10:00:00Z,2026-11-17T10:00:00Z,,allRepos",
		",ci,app-release,grant,writer,raw/app,,2026-11-23T10:00:00Z,,,",
		",ci,mirror,grant,reader,*/*,,,,,allRepos;noExpiry",
	}, "\n") + "\n"
	if buf.String() != want {
		t.Errorf("the CSV\n%s\nwant\n%s", buf.String(), want)
	}
}

// TestAccessReportCSVFormula: a cell that a spreadsheet would take for a formula, the name of a
// user or of a token that starts with =, +, -, @, a tab or a carriage return, gets a ' before it;
// the JSON document keeps it as it is.
func TestAccessReportCSVFormula(t *testing.T) {
	a := accessRows()
	a.Users[3].Name = `=HYPERLINK("https://evil.example.com")`
	a.Tokens[3].Name = "@SUM(1)"
	a.Bindings = append(a.Bindings, index.Binding{ID: 7, Principal: "user:" + accessUID(4), Role: "reader", RepoMask: "raw/app"})
	rep := NewAccessReport(a, accessConfig, AccessQuery{Scope: auth.AccessAll, UnusedDays: 90})
	var buf bytes.Buffer
	must(t, rep.WriteCSV(&buf))
	for _, want := range []string{`,local,"'=HYPERLINK(""https://evil.example.com"")",binding,`, `,deploy,'@SUM(1),token,`} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("the CSV has no %s:\n%s", want, buf.String())
		}
	}
	for _, s := range []string{",=", ",@", `,"=`} {
		if strings.Contains(buf.String(), s) {
			t.Errorf("the CSV has a cell that starts %q:\n%s", s[1:], buf.String())
		}
	}
	doc, err := rep.JSON()
	must(t, err)
	if !bytes.Contains(doc, []byte(`"name":"=HYPERLINK(\"https://evil.example.com\")"`)) {
		t.Errorf("the document changed the name:\n%s", doc)
	}
}

// TestAccessDigest: the sha256 of a document, as sha256:<hex>.
func TestAccessDigest(t *testing.T) {
	if got := AccessDigest([]byte("{}\n")); got != "sha256:ca3d163bab055381827226140568f3bef7eaac187cebd76878e0b63e9e442356" {
		t.Errorf("AccessDigest = %s", got)
	}
}
