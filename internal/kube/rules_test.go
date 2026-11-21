package kube

import (
	"reflect"
	"strings"
	"testing"
)

// The rule list reaches the Job as /in/rules.json behind PERGENT_RULES, never
// as an argument: an older image ignores the variable and exits 2 on a flag.
func TestJobRulesEnv(t *testing.T) {
	p := testParams()
	p.Inputs.Rules = []byte(`[{"source":"repo","path":"*.go","rule":"x"}]`)
	job := BuildJob(p)
	main := containerByName(t, job.Spec.Template.Spec.Containers, "pergent")
	if got, ok := envValue(main.Env, "PERGENT_RULES"); !ok || got != "/in/rules.json" {
		t.Errorf("PERGENT_RULES = %q, %v", got, ok)
	}
	for _, a := range main.Args {
		if strings.Contains(a, "rules") {
			t.Errorf("unexpected arg %q", a)
		}
	}
	var item bool
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.ConfigMap == nil {
			continue
		}
		for _, it := range v.ConfigMap.Items {
			item = item || (it.Key == "rules.json" && it.Path == "rules.json")
		}
	}
	if !item {
		t.Error("the inputs volume has no rules.json item")
	}
	if cm := BuildConfigMap(p, job); cm.Data["rules.json"] != string(p.Inputs.Rules) {
		t.Errorf("ConfigMap rules.json = %q", cm.Data["rules.json"])
	}
}

func TestJobNoRulesUnchanged(t *testing.T) {
	p := testParams()
	before := BuildJob(p)
	p.Inputs.Rules = nil
	p.NoBuiltInRules = false
	if !reflect.DeepEqual(BuildJob(p), before) {
		t.Error("a Job with no rules differs from one built before")
	}
	for _, e := range containerByName(t, before.Spec.Template.Spec.Containers, "pergent").Env {
		if e.Name == "PERGENT_RULES" || e.Name == "PERGENT_NO_BUILT_IN_RULES" {
			t.Errorf("%s set with no rules", e.Name)
		}
	}
	if _, ok := BuildConfigMap(p, before).Data["rules.json"]; ok {
		t.Error("ConfigMap has rules.json with no rules")
	}
}

func TestJobNoBuiltInRules(t *testing.T) {
	p := testParams()
	p.NoBuiltInRules = true
	main := containerByName(t, BuildJob(p).Spec.Template.Spec.Containers, "pergent")
	var got []string
	for _, e := range main.Env {
		if e.Name == "PERGENT_NO_BUILT_IN_RULES" {
			got = append(got, e.Value)
		}
	}
	if len(got) != 1 || got[0] != "true" {
		t.Errorf("PERGENT_NO_BUILT_IN_RULES = %q, want one \"true\"", got)
	}
	for _, a := range main.Args {
		if strings.Contains(a, "built-in") {
			t.Errorf("unexpected arg %q", a)
		}
	}
}
