package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckCommand_WorkspaceSingleSkill(t *testing.T) {
	dir := t.TempDir()
	skillContent := `---
name: ws-single-skill
description: A test skill for workspace-aware check command.
---

# Workspace Single Skill

Body content.
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(skillContent), 0o644))
	t.Chdir(dir)

	cmd := newCheckCommand()
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs(nil) // no args — workspace detection

	err := cmd.Execute()
	require.NoError(t, err)

	result := output.String()
	assert.Contains(t, result, "ws-single-skill")
	assert.Contains(t, result, "Compliance Score:")
}

func TestCheckCommand_WorkspaceMultiSkill(t *testing.T) {
	dir := t.TempDir()
	skillsDir := filepath.Join(dir, "skills")

	for _, name := range []string{"skill-one", "skill-two"} {
		skillDir := filepath.Join(skillsDir, name)
		require.NoError(t, os.MkdirAll(skillDir, 0o755))
		content := "---\nname: " + name + "\ndescription: \"A test skill description.\"\n---\n# Body\n"
		require.NoError(t, os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0o644))
	}
	t.Chdir(dir)

	cmd := newCheckCommand()
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs(nil) // no args — multi-skill workspace detection

	err := cmd.Execute()
	require.NoError(t, err)

	result := output.String()
	assert.Contains(t, result, "=== skill-one ===")
	assert.Contains(t, result, "=== skill-two ===")
	assert.Contains(t, result, "CHECK SUMMARY")
}

func TestCheckCommand_WorkspaceByName(t *testing.T) {
	dir := t.TempDir()
	skillsDir := filepath.Join(dir, "skills")

	for _, name := range []string{"named-skill", "other-skill"} {
		skillDir := filepath.Join(skillsDir, name)
		require.NoError(t, os.MkdirAll(skillDir, 0o755))
		content := "---\nname: " + name + "\ndescription: \"A test skill description.\"\n---\n# Body\n"
		require.NoError(t, os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0o644))
	}
	t.Chdir(dir)

	cmd := newCheckCommand()
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs([]string{"named-skill"}) // skill name

	err := cmd.Execute()
	require.NoError(t, err)

	result := output.String()
	assert.Contains(t, result, "named-skill")
	// Should NOT contain the other skill or multi-skill summary
	assert.NotContains(t, result, "CHECK SUMMARY")
}

func TestCheckCommand_ExplicitPathStillWorks(t *testing.T) {
	// Backward compatibility: explicit path arg works as before
	tmpDir := t.TempDir()
	skillContent := `---
name: path-skill
description: A skill with explicit path for backward compat test.
---

# Path Skill

Body content.
`
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "SKILL.md"), []byte(skillContent), 0o644))

	cmd := newCheckCommand()
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs([]string{tmpDir}) // explicit path

	err := cmd.Execute()
	require.NoError(t, err)

	result := output.String()
	assert.Contains(t, result, "path-skill")
	assert.Contains(t, result, "Compliance Score:")
}

func TestCheckCommand_WorkspaceWithSeparatedEval(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "skills", "eval-test")
	require.NoError(t, os.MkdirAll(skillDir, 0o755))
	content := "---\nname: eval-test\ndescription: \"Testing eval detection.\"\n---\n# Body\n"
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0o644))

	// Create separated eval: {root}/evals/{skill-name}/eval.yaml
	evalsDir := filepath.Join(dir, "evals", "eval-test")
	require.NoError(t, os.MkdirAll(evalsDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(evalsDir, "eval.yaml"), []byte("name: test\n"), 0o644))
	t.Chdir(dir)

	cmd := newCheckCommand()
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs([]string{"eval-test"})

	err := cmd.Execute()
	require.NoError(t, err)

	result := output.String()
	assert.Contains(t, result, "Evaluation Suite: Found")
}

func TestCheckCommand_WorkspaceByNameNestedSkillWithSeparatedEval(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".waza.yaml"), []byte("paths:\n  skills: skills/\n  evals: evals/\n"), 0o644))

	skillDir := filepath.Join(dir, "skills", "development", "skill-creator")
	require.NoError(t, os.MkdirAll(skillDir, 0o755))
	content := "---\nname: skill-creator\ndescription: \"Testing nested eval detection.\"\n---\n# Body\n"
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0o644))

	evalsDir := filepath.Join(dir, "evals", "skill-creator")
	require.NoError(t, os.MkdirAll(evalsDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(evalsDir, "eval.yaml"), []byte("name: test\nskill: skill-creator\n"), 0o644))
	t.Chdir(dir)

	cmd := newCheckCommand()
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs([]string{"skill-creator"})

	err := cmd.Execute()
	require.NoError(t, err)

	result := output.String()
	assert.Contains(t, result, "skill-creator")
	assert.Contains(t, result, "Evaluation Suite: Found")
	assert.NotContains(t, result, "no SKILL.md found")
}

func TestCheckCommand_ExplicitNestedPathWithSeparatedEval(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".waza.yaml"), []byte("paths:\n  skills: skills/\n  evals: evals/\n"), 0o644))

	skillDir := filepath.Join(dir, "skills", "development", "skill-creator")
	require.NoError(t, os.MkdirAll(skillDir, 0o755))
	content := "---\nname: skill-creator\ndescription: \"Testing explicit nested path eval detection.\"\n---\n# Body\n"
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0o644))

	evalsDir := filepath.Join(dir, "evals", "skill-creator")
	require.NoError(t, os.MkdirAll(evalsDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(evalsDir, "eval.yaml"), []byte("name: test\nskill: skill-creator\n"), 0o644))
	t.Chdir(dir)

	cmd := newCheckCommand()
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs([]string{filepath.Join("skills", "development", "skill-creator")})

	err := cmd.Execute()
	require.NoError(t, err)

	result := output.String()
	assert.Contains(t, result, "skill-creator")
	assert.Contains(t, result, "Evaluation Suite: Found")
}
