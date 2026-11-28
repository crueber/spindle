package importer

import (
	"sort"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/threads/workflow"
)

// top wraps top-level YAML above a minimal valid job.
func top(extra string) string {
	return extra + `
jobs:
  build:
    runs-on: linux
    steps:
      - run: "true"
`
}

// jobDoc wraps one job body.
func jobDoc(job string) string {
	return "on: push\njobs:\n  build:\n" + job
}

type translateCase struct {
	name string
	src  string
	// wantUntranslated is the exact set of refusal paths (sorted by the test).
	wantUntranslated []string
	// wantNotes asserts a note exists for at least this path prefix.
	wantNotes []string
	// contain/absent are substrings of the output.
	contain []string
	absent  []string
	// wantErr: Translate errors (the file cannot become a valid workflow).
	wantErr bool
}

func TestTranslateRefusals(t *testing.T) {
	cases := []translateCase{
		{
			name:             "scalar unknown trigger",
			src:              top("on: page_build\n"),
			wantUntranslated: []string{"on.page_build"},
			contain:          []string{"# UNTRANSLATED: on.page_build"},
		},
		{
			name:             "list unknown trigger",
			src:              top("on: [push, page_build]\n"),
			wantUntranslated: []string{"on.page_build"},
			contain:          []string{"on:\n  # UNTRANSLATED: on.page_build", "- push"},
		},
		{
			name:             "bare schedule in list",
			src:              top("on: [schedule]\n"),
			wantUntranslated: []string{"on.schedule"},
		},
		{
			name:             "scalar schedule",
			src:              top("on: schedule\n"),
			wantUntranslated: []string{"on.schedule"},
		},
		{
			name:             "unknown trigger mapping",
			src:              top("on:\n  page_build:\n    types: [x]\n"),
			wantUntranslated: []string{"on.page_build"},
			absent:           []string{"\n  page_build:"},
		},

		{
			name:             "push filter wrong shape",
			src:              top("on:\n  push:\n    branches:\n      a: b\n"),
			wantUntranslated: []string{"on.push.branches"},
		},
		{
			name:             "push trigger not a mapping",
			src:              top("on:\n  push: every-branch\n"),
			wantUntranslated: []string{"on.push"},
			absent:           []string{"push:"},
		},
		{
			name:             "pr type filtered",
			src:              top("on:\n  pull_request:\n    types: [opened, synchronize, deleted]\n"),
			wantUntranslated: []string{"on.pull_request.types"},
			contain:          []string{"types:", "- opened", "- synchronize"},
		},
		{
			name:             "pr types all refused",
			src:              top("on:\n  pull_request:\n    types: [deleted]\n"),
			wantUntranslated: []string{"on.pull_request.types", "on.pull_request.types"},
			absent:           []string{"\n    types:"},
		},
		{
			name:             "pr types not a list",
			src:              top("on:\n  pull_request:\n    types: opened\n"),
			wantUntranslated: []string{"on.pull_request.types"},
		},

		{
			name:             "pr unknown key",
			src:              top("on:\n  pull_request:\n    labels: [x]\n"),
			wantUntranslated: []string{"on.pull_request.labels"},
		},
		{
			name:             "pr trigger not a mapping",
			src:              top("on:\n  pull_request: yes\n"),
			wantUntranslated: []string{"on.pull_request"},
		},
		{
			name:             "dispatch unknown key",
			src:              top("on:\n  workflow_dispatch:\n    schedule: x\n"),
			wantUntranslated: []string{"on.workflow_dispatch.schedule"},
		},
		{
			name:             "dispatch not a mapping",
			src:              top("on:\n  workflow_dispatch: on\n"),
			wantUntranslated: []string{"on.workflow_dispatch"},
		},
		{
			name:             "dispatch input type",
			src:              top("on:\n  workflow_dispatch:\n    inputs:\n      env:\n        type: colour\n        options: [a, b]\n"),
			wantUntranslated: []string{"on.workflow_dispatch.inputs.env.type"},
			contain:          []string{"type: string", "options: [a, b]"},
			wantNotes:        []string{"on.workflow_dispatch.inputs.env.type"},
		},
		{
			name:             "dispatch input unknown key",
			src:              top("on:\n  workflow_dispatch:\n    inputs:\n      x:\n        deprecated: true\n"),
			wantUntranslated: []string{"on.workflow_dispatch.inputs.x.deprecated"},
		},
		{
			name:             "dispatch input not a mapping",
			src:              top("on:\n  workflow_dispatch:\n    inputs:\n      x: yes\n"),
			wantUntranslated: []string{"on.workflow_dispatch.inputs.x"},
		},
		{
			name:             "dispatch inputs not a mapping",
			src:              top("on:\n  workflow_dispatch:\n    inputs: [a]\n"),
			wantUntranslated: []string{"on.workflow_dispatch.inputs"},
		},
		{
			name:             "dispatch input options wrong shape",
			src:              top("on:\n  workflow_dispatch:\n    inputs:\n      x:\n        type: choice\n        options: one\n"),
			wantUntranslated: []string{"on.workflow_dispatch.inputs.x.options", "on.workflow_dispatch.inputs.x.type"},
		},
		{
			name:             "dispatch input option not scalar",
			src:              top("on:\n  workflow_dispatch:\n    inputs:\n      x:\n        type: choice\n        options:\n          - {a: b}\n"),
			wantUntranslated: []string{"on.workflow_dispatch.inputs.x.options", "on.workflow_dispatch.inputs.x.type"},
		},
		{
			name:             "dispatch input description wrong shape",
			src:              top("on:\n  workflow_dispatch:\n    inputs:\n      x:\n        description:\n          a: b\n"),
			wantUntranslated: []string{"on.workflow_dispatch.inputs.x.description"},
		},
		{
			name:             "dispatch input type wrong shape",
			src:              top("on:\n  workflow_dispatch:\n    inputs:\n      x:\n        type:\n          a: b\n"),
			wantUntranslated: []string{"on.workflow_dispatch.inputs.x.type"},
		},
		{
			name:             "bad cron",
			src:              top("on:\n  schedule:\n    - cron: '0 3 * * *'\n    - cron: 'not a cron'\n"),
			wantUntranslated: []string{"on.schedule[2]"},
			contain:          []string{"cron: '0 3 * * *'"},
		},
		{
			name:             "all crons refused",
			src:              top("on:\n  schedule:\n    - cron: 'not a cron'\n"),
			wantUntranslated: []string{"on.schedule", "on.schedule[1]"},
			absent:           []string{"\n  schedule:"},
		},
		{
			name:             "schedule entry not a mapping",
			src:              top("on:\n  schedule:\n    - '0 3 * * *'\n"),
			wantUntranslated: []string{"on.schedule", "on.schedule[1]"},
		},
		{
			name:             "schedule entry extra key",
			src:              top("on:\n  schedule:\n    - cron: '0 3 * * *'\n      tz: UTC\n"),
			wantUntranslated: []string{"on.schedule", "on.schedule[1]"},
		},
		{
			name:             "schedule cron not scalar",
			src:              top("on:\n  schedule:\n    - cron:\n        a: b\n"),
			wantUntranslated: []string{"on.schedule", "on.schedule[1]"},
		},
		{
			name:             "schedule not a list",
			src:              top("on:\n  schedule: '0 3 * * *'\n"),
			wantUntranslated: []string{"on.schedule"},
		},
		{
			name:             "on is a number",
			src:              top("on: 42\n"),
			wantUntranslated: []string{"on.42"},
		},
		{
			name:             "on is a list",
			src:              top("on:\n  - {a: b}\n"),
			wantUntranslated: []string{"on."},
		},
		{
			name:             "on wrong kind",
			src:              top("on:\n  - push\n  - 7\n"),
			wantUntranslated: []string{"on.7"},
		},
		{
			name:             "permissions unsupported scope is refused by name",
			src:              top("on: push\npermissions:\n  contents: read\n  packages: write\n"),
			wantUntranslated: []string{"permissions.packages"},
			contain:          []string{"contents: read"},
		},
		{
			name:             "permissions value wrong shape",
			src:              top("on: push\npermissions:\n  issues:\n    a: b\n"),
			wantUntranslated: []string{"permissions.issues"},
		},
		{
			name:             "permissions list",
			src:              top("on: push\npermissions: [a, b]\n"),
			wantUntranslated: []string{"permissions"},
		},
		{
			name:             "concurrency unsupported context",
			src:              top("on: push\nconcurrency:\n  group: ci-${{ env.BRANCH }}\n"),
			wantUntranslated: []string{"concurrency"},
			absent:           []string{"\nconcurrency:"},
		},
		{
			name:             "job concurrency wrong shape",
			src:              jobDoc("    runs-on: linux\n    concurrency: [a]\n    steps:\n      - run: 'true'\n"),
			wantUntranslated: []string{"jobs.build.concurrency"},
		},
		{
			name:             "concurrency cancel expression",
			src:              top("on: push\nconcurrency:\n  group: ci-${{ github.ref }}\n  cancel-in-progress: ${{ github.ref != 'refs/heads/main' }}\n"),
			wantUntranslated: []string{"concurrency.cancel-in-progress"},
			contain:          []string{"group: ci-${{ github.ref }}"},
		},
		{
			name:             "concurrency group wrong shape",
			src:              top("on: push\nconcurrency:\n  group:\n    a: b\n"),
			wantUntranslated: []string{"concurrency.group"},
			absent:           []string{"\nconcurrency:"},
		},
		{
			name:             "concurrency unknown key",
			src:              top("on: push\nconcurrency:\n  group: ci\n  queue: single\n"),
			wantUntranslated: []string{"concurrency.queue"},
		},
		{
			name:             "concurrency uses dispatch input",
			src:              top("on:\n  workflow_dispatch:\n    inputs:\n      mode:\n        type: string\nconcurrency:\n  group: ci-${{ inputs.mode }}\n"),
			contain:          []string{"group: ci-${{ inputs.mode }}"},
			wantUntranslated: nil,
		},
		{
			name:             "job concurrency uses matrix axis",
			src:              jobDoc("    runs-on: linux\n    strategy:\n      matrix:\n        os: [linux, darwin]\n    concurrency:\n      group: ci-${{ matrix.os }}\n    steps:\n      - run: 'true'\n"),
			contain:          []string{"group: ci-${{ matrix.os }}"},
			wantUntranslated: nil,
		},
		{
			name:             "job concurrency undeclared axis",
			src:              jobDoc("    runs-on: linux\n    concurrency:\n      group: ci-${{ matrix.os }}\n    steps:\n      - run: 'true'\n"),
			wantUntranslated: []string{"jobs.build.concurrency"},
		},
		{
			name:             "name wrong shape",
			src:              top("name:\n  a: b\non: push\n"),
			wantUntranslated: []string{"name"},
		},
		{
			name:             "env not a mapping",
			src:              top("on: push\nenv: [A=1]\n"),
			wantUntranslated: []string{"env"},
		},
		{
			name:             "env nested value",
			src:              top("on: push\nenv:\n  A:\n    b: c\n"),
			wantUntranslated: []string{"env.A"},
		},
		{
			name:    "jobs missing",
			src:     "on: push\nname: x\n",
			wantErr: true,
		},
		{
			name:    "jobs not a mapping",
			src:     "on: push\njobs: [a]\n",
			wantErr: true,
		},
		{
			name:    "jobs empty",
			src:     "on: push\njobs: {}\n",
			wantErr: true,
		},
		{
			name:             "job not a mapping",
			src:              "on: push\njobs:\n  build: nope\n",
			wantUntranslated: []string{"jobs.build"},
			contain:          []string{"UNTRANSLATED: jobs.build", "exit 1"},
		},
		{
			name:             "reusable job with a foreign reference",
			src:              "on: push\njobs:\n  call:\n    uses: ./.github/workflows/other.yml\n",
			wantUntranslated: []string{"jobs.call.uses"},
			contain:          []string{"UNTRANSLATED: jobs.call.uses", "exit 1"},
		},
		{
			name:             "reusable job with a mutable ref",
			src:              "on: push\njobs:\n  call:\n    uses: acme/other/.spindle/workflows/x.yml@main\n",
			wantUntranslated: []string{"jobs.call.uses"},
			contain:          []string{"40-character commit SHA"},
		},
		{
			name:             "reusable job with a caller key",
			src:              "on: push\njobs:\n  call:\n    uses: ./.spindle/workflows/other.yml\n    runs-on: linux\n",
			wantUntranslated: []string{"jobs.call.uses"},
			contain:          []string{"not allowed on a reusable-workflow caller job"},
		},
		{
			name:             "job secrets",
			src:              jobDoc("    runs-on: linux\n    secrets: inherit\n    steps:\n      - run: 'true'\n"),
			wantUntranslated: []string{"jobs.build.secrets"},
		},
		{
			name:             "job unknown key",
			src:              jobDoc("    runs-on: linux\n    strategy2: x\n    steps:\n      - run: 'true'\n"),
			wantUntranslated: []string{"jobs.build.strategy2"},
		},
		{
			name:             "job name wrong shape",
			src:              jobDoc("    name:\n      a: b\n    runs-on: linux\n    steps:\n      - run: 'true'\n"),
			wantUntranslated: []string{"jobs.build.name"},
		},
		{
			name:             "runner label list",
			src:              jobDoc("    runs-on: [docker, arm]\n    steps:\n      - run: 'true'\n"),
			contain:          []string{"self-hosted", "linux", "- docker", "- arm"},
			wantNotes:        []string{"jobs.build.runs-on"},
			wantUntranslated: nil,
		},
		{
			name:             "runner label wrong item",
			src:              jobDoc("    runs-on: [docker, {a: b}]\n    steps:\n      - run: 'true'\n"),
			wantUntranslated: []string{"jobs.build.runs-on"},
			contain:          []string{"self-hosted"},
		},
		{
			name:             "runner label mapping",
			src:              jobDoc("    runs-on:\n      group: g\n      labels: [a]\n    steps:\n      - run: 'true'\n"),
			wantUntranslated: []string{"jobs.build.runs-on"},
			contain:          []string{"self-hosted"},
		},
		{
			name:             "runner label empty",
			src:              jobDoc("    runs-on:\n    steps:\n      - run: 'true'\n"),
			wantUntranslated: []string{"jobs.build.runs-on"},
			wantNotes:        []string{"jobs.build.runs-on"},
		},
		{
			name:             "environment bad name",
			src:              jobDoc("    runs-on: linux\n    environment: ${{ matrix.env }}\n    steps:\n      - run: 'true'\n"),
			wantUntranslated: []string{"jobs.build.environment"},
		},
		{
			name:             "environment url expression",
			src:              jobDoc("    runs-on: linux\n    environment:\n      name: prod\n      url: ${{ steps.deploy.outputs.url }}\n    steps:\n      - run: 'true'\n"),
			wantUntranslated: []string{"jobs.build.environment"},
		},
		{
			name:             "environment name wrong shape",
			src:              jobDoc("    runs-on: linux\n    environment:\n      name: [prod]\n    steps:\n      - run: 'true'\n"),
			wantUntranslated: []string{"jobs.build.environment", "jobs.build.environment.name"},
		},
		{
			name:             "environment unknown key",
			src:              jobDoc("    runs-on: linux\n    environment:\n      name: prod\n      reviewers: [a]\n    steps:\n      - run: 'true'\n"),
			wantUntranslated: []string{"jobs.build.environment.reviewers"},
			contain:          []string{"name: prod"},
		},
		{
			name:             "environment wrong kind",
			src:              jobDoc("    runs-on: linux\n    environment: [prod]\n    steps:\n      - run: 'true'\n"),
			wantUntranslated: []string{"jobs.build.environment"},
		},
		{
			name:             "container credentials",
			src:              jobDoc("    runs-on: linux\n    container:\n      image: golang:1.27\n      credentials:\n        username: u\n        password: p\n    steps:\n      - run: 'true'\n"),
			wantUntranslated: []string{"jobs.build.container.credentials"},
			contain:          []string{"image: golang:1.27"},
		},
		{
			name:      "container cmd renamed",
			src:       jobDoc("    runs-on: linux\n    container:\n      image: golang:1.27\n      cmd: [go, version]\n    steps:\n      - run: 'true'\n"),
			wantNotes: []string{"jobs.build.container.cmd"},
			contain:   []string{"command:"},
		},
		{
			name:             "container image wrong shape",
			src:              jobDoc("    runs-on: linux\n    container:\n      image:\n        a: b\n    steps:\n      - run: 'true'\n"),
			wantUntranslated: []string{"jobs.build.container.image"},
		},
		{
			name:             "container options wrong shape",
			src:              jobDoc("    runs-on: linux\n    container:\n      image: x\n      options:\n        a: b\n    steps:\n      - run: 'true'\n"),
			wantUntranslated: []string{"jobs.build.container.options"},
		},
		{
			name:             "container unknown key",
			src:              jobDoc("    runs-on: linux\n    container:\n      image: x\n      cpus: 2\n    steps:\n      - run: 'true'\n"),
			wantUntranslated: []string{"jobs.build.container.cpus"},
		},
		{
			name:             "container not a shape",
			src:              jobDoc("    runs-on: linux\n    container: [x]\n    steps:\n      - run: 'true'\n"),
			wantUntranslated: []string{"jobs.build.container"},
		},
		{
			name:             "service cmd and credentials",
			src:              jobDoc("    runs-on: linux\n    services:\n      db:\n        image: postgres:16\n        cmd: [postgres]\n        credentials:\n          username: u\n    steps:\n      - run: 'true'\n"),
			wantUntranslated: []string{"jobs.build.services.db.credentials"},
			wantNotes:        []string{"jobs.build.services.db.cmd"},
			contain:          []string{"command:"},
		},
		{
			name:             "service not a mapping",
			src:              jobDoc("    runs-on: linux\n    services:\n      db: nope\n    steps:\n      - run: 'true'\n"),
			wantUntranslated: []string{"jobs.build.services.db"},
		},
		{
			name:             "services not a mapping",
			src:              jobDoc("    runs-on: linux\n    services: [db]\n    steps:\n      - run: 'true'\n"),
			wantUntranslated: []string{"jobs.build.services"},
		},
		{
			name:             "service image wrong shape",
			src:              jobDoc("    runs-on: linux\n    services:\n      db:\n        image:\n          a: b\n    steps:\n      - run: 'true'\n"),
			wantUntranslated: []string{"jobs.build.services.db.image"},
		},
		{
			name:             "service unknown key",
			src:              jobDoc("    runs-on: linux\n    services:\n      db:\n        image: x\n        healthcheck: y\n    steps:\n      - run: 'true'\n"),
			wantUntranslated: []string{"jobs.build.services.db.healthcheck"},
		},
		{
			name:             "needs non-scalar item",
			src:              "on: push\njobs:\n  a:\n    runs-on: linux\n    steps:\n      - run: 'true'\n  build:\n    runs-on: linux\n    needs: [a, {b: c}]\n    steps:\n      - run: 'true'\n",
			wantUntranslated: []string{"jobs.build.needs"},
			contain:          []string{"needs:", "- a"},
		},
		{
			name:             "needs wrong shape",
			src:              jobDoc("    runs-on: linux\n    needs:\n      a: b\n    steps:\n      - run: 'true'\n"),
			wantUntranslated: []string{"jobs.build.needs"},
		},
		{
			name:             "job if wrong shape",
			src:              jobDoc("    runs-on: linux\n    if:\n      a: b\n    steps:\n      - run: 'true'\n"),
			wantUntranslated: []string{"jobs.build.if"},
		},
		{
			name:             "job timeout expression",
			src:              jobDoc("    runs-on: linux\n    timeout-minutes: ${{ matrix.minutes }}\n    steps:\n      - run: 'true'\n"),
			wantUntranslated: []string{"jobs.build.timeout-minutes"},
		},
		{
			name:             "job continue-on-error expression",
			src:              jobDoc("    runs-on: linux\n    continue-on-error: ${{ matrix.experimental }}\n    steps:\n      - run: 'true'\n"),
			wantUntranslated: []string{"jobs.build.continue-on-error"},
		},
		{
			name:             "strategy not a mapping",
			src:              jobDoc("    runs-on: linux\n    strategy: [a]\n    steps:\n      - run: 'true'\n"),
			wantUntranslated: []string{"jobs.build.strategy"},
		},
		{
			name:             "matrix not a mapping",
			src:              jobDoc("    runs-on: linux\n    strategy:\n      matrix: [a]\n    steps:\n      - run: 'true'\n"),
			wantUntranslated: []string{"jobs.build.strategy.matrix"},
		},
		{
			name:             "matrix object axis",
			src:              jobDoc("    runs-on: linux\n    strategy:\n      matrix:\n        os:\n          - {name: linux}\n    steps:\n      - run: 'true'\n"),
			wantUntranslated: []string{"jobs.build.strategy.matrix.os"},
			contain:          []string{"# UNTRANSLATED: jobs.build.strategy.matrix.os", "untranslated-object-cell", "see the UNTRANSLATED comments"},
		},
		{
			name:             "matrix include not a list",
			src:              jobDoc("    runs-on: linux\n    strategy:\n      matrix:\n        include: nope\n    steps:\n      - run: 'true'\n"),
			wantUntranslated: []string{"jobs.build.strategy.matrix.include"},
			absent:           []string{"\n        include:"},
			contain:          []string{"see the UNTRANSLATED comments"},
		},
		{
			name:             "matrix include entry not an object",
			src:              jobDoc("    runs-on: linux\n    strategy:\n      matrix:\n        include:\n          - nope\n    steps:\n      - run: 'true'\n"),
			wantUntranslated: []string{"jobs.build.strategy.matrix.include[1]"},
			contain:          []string{"see the UNTRANSLATED comments"},
		},
		{
			name:             "matrix include nested value",
			src:              jobDoc("    runs-on: linux\n    strategy:\n      matrix:\n        include:\n          - os:\n              name: linux\n    steps:\n      - run: 'true'\n"),
			wantUntranslated: []string{"jobs.build.strategy.matrix.include[1].os"},
			contain:          []string{"os: untranslated-object-cell", "see the UNTRANSLATED comments"},
		},
		{
			name:             "matrix include empty key",
			src:              jobDoc("    runs-on: linux\n    strategy:\n      matrix:\n        include:\n          - '': x\n    steps:\n      - run: 'true'\n"),
			wantUntranslated: []string{"jobs.build.strategy.matrix.include[1]"},
			contain:          []string{"see the UNTRANSLATED comments"},
		},
		{
			name:             "matrix object cell",
			src:              jobDoc("    runs-on: linux\n    strategy:\n      matrix:\n        os:\n          - {name: linux}\n    steps:\n      - run: 'true'\n"),
			wantUntranslated: []string{"jobs.build.strategy.matrix.os"},
		},
		{
			name:             "strategy fail-fast expression",
			src:              jobDoc("    runs-on: linux\n    strategy:\n      fail-fast: ${{ inputs.ff }}\n      matrix:\n        os: [a]\n    steps:\n      - run: 'true'\n"),
			wantUntranslated: []string{"jobs.build.strategy.fail-fast"},
		},
		{
			name:             "strategy unknown key",
			src:              jobDoc("    runs-on: linux\n    strategy:\n      matrix:\n        os: [a]\n      queue: x\n    steps:\n      - run: 'true'\n"),
			wantUntranslated: []string{"jobs.build.strategy.queue"},
		},
		{
			name:             "steps not a list",
			src:              jobDoc("    runs-on: linux\n    steps: nope\n"),
			wantUntranslated: []string{"jobs.build.steps"},
			contain:          []string{"UNTRANSLATED", "exit 1"},
		},
		{
			name:             "step not a mapping",
			src:              jobDoc("    runs-on: linux\n    steps:\n      - nope\n"),
			wantUntranslated: []string{"jobs.build.steps[1]"},
			contain:          []string{"exit 1"},
		},
		{
			name:             "step unknown key",
			src:              jobDoc("    runs-on: linux\n    steps:\n      - run: 'true'\n        background: true\n"),
			wantUntranslated: []string{"jobs.build.steps[1].background"},
		},
		{
			name:             "step id wrong shape",
			src:              jobDoc("    runs-on: linux\n    steps:\n      - id:\n          a: b\n        run: 'true'\n"),
			wantUntranslated: []string{"jobs.build.steps[1].id"},
		},
		{
			name:             "step name wrong shape",
			src:              jobDoc("    runs-on: linux\n    steps:\n      - name:\n          a: b\n        run: 'true'\n"),
			wantUntranslated: []string{"jobs.build.steps[1].name"},
		},
		{
			name:             "step run wrong shape",
			src:              jobDoc("    runs-on: linux\n    steps:\n      - run:\n          a: b\n"),
			wantUntranslated: []string{"jobs.build.steps[1]", "jobs.build.steps[1].run"},
			contain:          []string{"exit 1"},
		},
		{
			name:             "step uses wrong shape",
			src:              jobDoc("    runs-on: linux\n    steps:\n      - uses:\n          a: b\n"),
			wantUntranslated: []string{"jobs.build.steps[1]", "jobs.build.steps[1].uses"},
			contain:          []string{"exit 1"},
		},
		{
			name:             "step with not a mapping",
			src:              jobDoc("    runs-on: linux\n    steps:\n      - uses: actions/checkout\n        with: [a]\n"),
			wantUntranslated: []string{"jobs.build.steps[1].with"},
		},
		{
			name:             "step if wrong shape",
			src:              jobDoc("    runs-on: linux\n    steps:\n      - run: 'true'\n        if:\n          a: b\n"),
			wantUntranslated: []string{"jobs.build.steps[1].if"},
		},
		{
			name:             "step continue-on-error expression",
			src:              jobDoc("    runs-on: linux\n    steps:\n      - run: 'true'\n        continue-on-error: ${{ matrix.experimental }}\n"),
			wantUntranslated: []string{"jobs.build.steps[1].continue-on-error"},
		},
		{
			name:             "step timeout expression",
			src:              jobDoc("    runs-on: linux\n    steps:\n      - run: 'true'\n        timeout-minutes: ${{ inputs.t }}\n"),
			wantUntranslated: []string{"jobs.build.steps[1].timeout-minutes"},
		},
		{
			name:             "step both verbs",
			src:              jobDoc("    runs-on: linux\n    steps:\n      - run: 'true'\n        uses: actions/checkout\n"),
			wantUntranslated: []string{"jobs.build.steps[1].uses"},
			contain:          []string{"run: 'true'"},
		},
		{
			name:             "step no verb",
			src:              jobDoc("    runs-on: linux\n    steps:\n      - name: nothing\n"),
			wantUntranslated: []string{"jobs.build.steps[1]"},
			contain:          []string{"exit 1"},
		},
		{
			name:             "remote action refused",
			src:              jobDoc("    runs-on: linux\n    steps:\n      - uses: actions/setup-go@v5\n"),
			wantUntranslated: []string{"jobs.build.steps[1].uses"},
			contain:          []string{"exit 1", "UNTRANSLATED: jobs.build.steps[1].uses"},
		},
		{
			name:             "remote action with run keeps run",
			src:              jobDoc("    runs-on: linux\n    steps:\n      - run: 'true'\n        uses: actions/setup-go@v5\n"),
			wantUntranslated: []string{"jobs.build.steps[1].uses"},
			contain:          []string{"run: 'true'"},
		},
		{
			name:             "no steps at all",
			src:              jobDoc("    runs-on: linux\n"),
			contain:          []string{"exit 1"},
			wantUntranslated: []string{"jobs.build.steps"},
		},
		{
			name:      "no runs-on",
			src:       "on: push\njobs:\n  build:\n    steps:\n      - run: 'true'\n",
			wantNotes: []string{"jobs.build.runs-on"},
			contain:   []string{"self-hosted", "linux"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Translate(Forgejo, []byte(tc.src))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Translate succeeded, want an error; output:\n%s", res.Output)
				}
				return
			}
			if err != nil {
				t.Fatalf("Translate: %v", err)
			}
			if _, perr := workflow.Parse(res.Output); perr != nil {
				t.Fatalf("the converted file does not parse: %v\n%s", perr, res.Output)
			}
			var got []string
			for _, item := range res.Untranslated {
				got = append(got, item.Path)
				if !strings.Contains(string(res.Output), "# UNTRANSLATED: "+item.Path) {
					t.Errorf("no UNTRANSLATED comment for %s in:\n%s", item.Path, res.Output)
				}
			}
			want := append([]string(nil), tc.wantUntranslated...)
			sort.Strings(got)
			sort.Strings(want)
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("untranslated paths = %v, want %v\nitems: %+v\noutput:\n%s", got, want, res.Untranslated, res.Output)
			}
			for _, c := range tc.contain {
				if !strings.Contains(string(res.Output), c) {
					t.Errorf("output does not contain %q:\n%s", c, res.Output)
				}
			}
			for _, a := range tc.absent {
				if strings.Contains(string(res.Output), a) {
					t.Errorf("output still contains %q:\n%s", a, res.Output)
				}
			}
			for _, p := range tc.wantNotes {
				found := false
				for _, n := range res.Notes {
					if strings.HasPrefix(n.Path, p) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("no note for %q; notes: %+v", p, res.Notes)
				}
			}
		})
	}
}
