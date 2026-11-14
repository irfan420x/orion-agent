# Orion — Makefile
#
#   make build     build orion + oriond into ./bin (with version ldflags)
#   make install   build and install to /usr/local/bin or ~/.local/bin
#                  (same rules as install.sh; prefer ./install.sh for
#                  the full flow including default config + --systemd)
#   make test      run the full test suite
#   make vet       go vet ./...
#   make fmt       gofmt -l (fails if any file needs formatting)
#   make version   print the version the next build will report
#   make release-check  release gate (Audit 7, F-A7-P2-7; Audit 8,
#                  F-A8-P2-4): build orion, assert `orion version`
#                  matches the exact-match git tag, assert a plain
#                  build without ldflags reports the release in
#                  preparation (the fallback in internal/version),
#                  assert CHANGELOG.md has a heading for the tag,
#                  and assert the README test counts match the suite
#                  (it runs the full suite itself). Fails loudly
#                  otherwise; do not push when it fails.
#   make clean     remove ./bin
#
# Build info (version from the exact-match git tag, commit + date)
# is baked in with -ldflags -X so `orion version` reports the real
# build. An untagged build reports the source default in
# internal/version/version.go (never "dev", never a stale tag).

GO ?= go
# Go toolchain resolution (rc24, Audit 18 §10 step 6; rc25 F-A19-7
# adds the audit toolchain): fall back to the per-user Go installs
# this VM/container setup is known to use when `go` is not on PATH
# (/usr/local/go does not survive restores here, so it is only a
# last resort), plus the auditor's toolchain
# ($(HOME)/workspace/orion-audit/toolchain/go/bin/go — the audit
# VM's Go lives there, and it was missing from this list, so `make`
# without GO failed at parse time on that VM). Each candidate is
# verified by actually RUNNING it (`go version`), not just by
# `command -v` / file existence — a stale, broken or wrong-arch
# binary must not be picked up silently. When no candidate runs,
# fail here with the locations checked, instead of failing later
# with a cryptic "go: command not found" from a recipe. An
# explicitly supplied GO (make GO=...) is honoured as-is: a
# command-line assignment cannot be reassigned by the fallback
# chain, so a GO that does not run fails below with the locations
# checked, instead of silently falling back to a different
# toolchain than the operator asked for. (A GO from the environment
# falls back like the default.)
#
# F-A19-7: the parse-time failure below must not break Go-free
# targets (`make clean`, `make version`) on a Go-less host, so it
# only fires when a goal actually needs Go. An empty goal list is
# the default target (release-check), which needs Go.
GO_FREE_GOALS := clean version
ifeq ($(strip $(MAKECMDGOALS)),)
_NEEDS_GO := yes
else
_NEEDS_GO := $(filter-out $(GO_FREE_GOALS),$(MAKECMDGOALS))
endif
ifeq ($(shell $(GO) version >/dev/null 2>&1 && echo ok),)
GO := $(HOME)/go1.27.4/bin/go
endif
ifeq ($(shell $(GO) version >/dev/null 2>&1 && echo ok),)
GO := /usr/local/go/bin/go
endif
ifeq ($(shell $(GO) version >/dev/null 2>&1 && echo ok),)
GO := $(HOME)/workspace/orion-audit/toolchain/go/bin/go
endif
ifeq ($(shell $(GO) version >/dev/null 2>&1 && echo ok),)
ifneq ($(_NEEDS_GO),)
$(error Go toolchain not found or not runnable: checked 'go' on PATH, $(HOME)/go1.27.4/bin/go, /usr/local/go/bin/go and $(HOME)/workspace/orion-audit/toolchain/go/bin/go (need Go 1.27+); install Go from https://go.dev/dl or pass a working toolchain with make GO=/path/to/go)
endif
endif
VERSION_PKG := github.com/irfan/orion/internal/version
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
# Version source of truth: the exact-match git tag (v0.2.0-rc11 -> 0.2.0-rc11),
# set via -ldflags -X. When HEAD is not exactly on a tag (an untagged
# commit between releases) TAG_VERSION is empty and no -X is passed,
# so the binary reports the source default in internal/version
# (the release this tree is working towards) — never "dev" and never
# a stale tag. See F-A6-P2-16: Version used to be a const stuck at
# rc9 while the tag was rc10, because ldflags cannot override a const.
TAG_VERSION := $(shell git describe --tags --exact-match 2>/dev/null | sed -E 's/^v//')
VERSION_DEFAULT := $(shell grep -E '^var Version' internal/version/version.go | sed -E 's/.*"([^"]+)".*/\1/')
EFFECTIVE_VERSION := $(if $(TAG_VERSION),$(TAG_VERSION),$(VERSION_DEFAULT))
LDFLAGS := -X $(VERSION_PKG).Commit=$(COMMIT) -X $(VERSION_PKG).BuildDate=$(DATE) $(if $(TAG_VERSION),-X $(VERSION_PKG).Version=$(TAG_VERSION))

BIN_DIR ?= bin

.PHONY: build install test vet fmt version clean release-check check-version check-version-fallback check-changelog check-readme-counts

# Release gate (Audit 7, F-A7-P2-7; Audit 8, F-A8-P2-4). The
# version-vs-tag check used to be a doc-only checklist item in
# CHANGELOG.md, and the rc10 binary reported rc9 because of it.
# `make release-check` automates the mechanically checkable parts:
# `orion version` == the exact-match git tag, CHANGELOG.md has a
# heading for the tag, and the README test counts match the suite
# (check-readme-counts runs the full suite itself, with TMPDIR=/tmp —
# the sandbox tests assume TMPDIR is under /tmp; see Audit 7 §1.1).
# It also builds orion WITHOUT ldflags and asserts the plain build
# reports the release this tree is working towards (the top
# `## [x.y.z]` heading of CHANGELOG.md) — the fallback in
# internal/version/version.go. The fallback is the version of the
# release in preparation, so it is compared against the top
# CHANGELOG heading, not the git tag: between tags the fallback is
# already the next release (rc13 while HEAD is still tagged rc12),
# and a tag comparison would fail for that whole window — which is
# exactly when the bump lands and exactly how the rc11 fallback
# survived the rc12 tag unnoticed. At gate time the top heading IS
# the tag (check-changelog enforces a heading for the tag), so the
# two comparisons coincide. It does NOT check: -race, gofmt, go
# vet, or the live DeepSeek proof — those stay manual checklist
# items in CHANGELOG.md. CHECK_BIN overrides which binary
# check-version runs against (default: the freshly built
# ./bin/orion); it exists so the check can be proven to fail on a
# deliberately mismatched binary.
RELEASE_TAG := $(shell git describe --tags --exact-match 2>/dev/null)
RELEASE_VERSION := $(RELEASE_TAG:v%=%)
CHECK_BIN ?= $(BIN_DIR)/orion

release-check: build check-version check-version-fallback check-changelog check-readme-counts
	@echo "release-check: PASS — orion version matches $(RELEASE_TAG), plain build reports the fallback release, CHANGELOG has the tag heading, README counts match the suite"

check-version:
	@test -n "$(RELEASE_TAG)" || { echo "release-check: FAIL — HEAD is not exactly on a git tag (git describe --tags --exact-match failed); tag the commit before release-check"; exit 1; }
	@bin="$(CHECK_BIN)"; case "$$bin" in /*) ;; *) bin="./$$bin";; esac; \
	test -x "$$bin" || { echo "release-check: FAIL — no executable binary at $$bin (run make build first)"; exit 1; }; \
	got="$$($$bin version | awk '{print $$2}')"; \
	test "$$got" = "$(RELEASE_VERSION)" || { echo "release-check: FAIL — orion version reports '$$got' but the tag is '$(RELEASE_TAG)' (expected '$(RELEASE_VERSION)'); do not push"; exit 1; }; \
	echo "release-check: orion version '$$got' matches tag '$(RELEASE_TAG)'"

# check-version-fallback (Audit 8, F-A8-P2-4): the fallback in
# internal/version/version.go was still 0.2.0-rc11 at the rc12 tag,
# so a plain `go build` binary misreported the version — and
# release-check never saw it, because it only ever ran the
# Makefile-built (ldflags) binary. This builds orion with NO ldflags
# and asserts the binary reports the release this tree is working
# towards: the version in the top `## [x.y.z]` heading of
# CHANGELOG.md. It must FAIL on a stale fallback (e.g. fallback
# rc11 under an rc13 heading) and PASS once the fallback is bumped.
# (Do NOT put `#` comment lines inside the recipe below: a comment
# line has no trailing backslash, so it splits the backslash-
# continued recipe into two shells and the second shell loses $tmp.)
check-version-fallback:
	@tmp="$$(mktemp -d)"; \
	trap 'rm -rf "$$tmp"' EXIT INT TERM; \
	$(GO) build -trimpath -o "$$tmp/orion-plain" ./cmd/orion || { echo "release-check: FAIL — plain build without ldflags failed"; exit 1; }; \
	got="$$("$$tmp/orion-plain" version | awk '{print $$2}')"; \
	want="$$(sed -n 's/^## \[\([0-9][^]]*\)\].*/\1/p' CHANGELOG.md | head -1)"; \
	test -n "$$want" || { echo "release-check: FAIL — no '## [x.y.z]' heading found in CHANGELOG.md"; exit 1; }; \
	test "$$got" = "$$want" || { echo "release-check: FAIL — plain build (no ldflags) reports '$$got' but the top CHANGELOG heading is '[$$want]'; bump the fallback in internal/version/version.go to $$want; do not push"; exit 1; }; \
	echo "release-check: plain build (no ldflags) reports '$$got', matching the top CHANGELOG heading '[$$want]'"

check-changelog:
	@test -n "$(RELEASE_TAG)" || { echo "release-check: FAIL — HEAD is not exactly on a git tag; tag the commit before release-check"; exit 1; }
	@grep -q "^## \[$(RELEASE_VERSION)\]" CHANGELOG.md || { echo "release-check: FAIL — CHANGELOG.md has no '## [$(RELEASE_VERSION)]' heading for the tag; retitle the [Unreleased] section first"; exit 1; }
	@echo "release-check: CHANGELOG.md has a '## [$(RELEASE_VERSION)]' heading"

# check-readme-counts parses the README counts with grep -o (no `.*`,
# so a greedy prefix cannot split a thousands-separated number — an
# earlier sed form parsed "1,105" as "5") and reduces them to bare
# digits with awk, which always ends the line with a newline: the
# tr/sed form left no trailing newline, so the two appended counts
# ran together into one unterminated line and POSIX `read` refused
# it (n=0). (Do NOT put `#` comment lines inside the recipe below:
# a comment line has no trailing backslash, so it splits the
# backslash-continued recipe into two shells and the second shell
# loses $tmp.)
check-readme-counts:
	@tmp="$$(mktemp -d)"; \
	if ! TMPDIR=/tmp $(GO) test -count=1 -v ./... >"$$tmp/test-v.log" 2>&1; then echo "release-check: FAIL — test suite did not pass; log kept at $$tmp/test-v.log"; tail -20 "$$tmp/test-v.log"; exit 1; fi; \
	pass=$$(grep -c '^--- PASS: ' "$$tmp/test-v.log" || true); \
	fail=$$(grep -c '^--- FAIL: ' "$$tmp/test-v.log" || true); \
	skip=$$(grep -c '^--- SKIP: ' "$$tmp/test-v.log" || true); \
	total=$$((pass + fail + skip)); \
	echo "release-check: suite counted $$total total ($$pass pass, $$fail fail, $$skip skip)"; \
	grep -o '([0-9][0-9,]* tests total — [0-9][0-9,]* pass, [0-9][0-9,]* fail, [0-9][0-9,]* skip' README.md | head -1 | tr -d ',' | tr -cs '0-9' ' ' | awk '{print $$1, $$2, $$3, $$4}' > "$$tmp/readme-counts.txt"; \
	grep -o '[0-9][0-9,]* tests total ([0-9][0-9,]* passing, [0-9][0-9,]* failing, [0-9][0-9,]* skipped' README.md | head -1 | tr -d ',' | tr -cs '0-9' ' ' | awk '{print $$1, $$2, $$3, $$4}' >> "$$tmp/readme-counts.txt"; \
	ok=1; n=0; \
	while read -r rtotal rpass rfail rskip; do \
		n=$$((n + 1)); \
		rtotal=$$(echo "$$rtotal" | tr -d ','); rpass=$$(echo "$$rpass" | tr -d ','); rfail=$$(echo "$$rfail" | tr -d ','); rskip=$$(echo "$$rskip" | tr -d ','); \
		if [ "$$rtotal" != "$$total" ] || [ "$$rpass" != "$$pass" ] || [ "$$rfail" != "$$fail" ] || [ "$$rskip" != "$$skip" ]; then \
			echo "release-check: FAIL — README count #$$n says $$rtotal total ($$rpass pass, $$rfail fail, $$rskip skip) but the suite counted $$total total ($$pass pass, $$fail fail, $$skip skip); update README first"; \
			ok=0; \
		fi; \
	done < "$$tmp/readme-counts.txt"; \
	test "$$n" = 2 || { echo "release-check: FAIL — could not parse both README test counts (found $$n); the banner and ## Tests formats changed"; ok=0; }; \
	test "$$ok" = 1 || exit 1; \
	rm -rf "$$tmp"; \
	echo "release-check: README test counts match the suite ($$total total, $$pass pass, $$fail fail, $$skip skip)"

build:
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/orion ./cmd/orion
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/oriond ./cmd/oriond
	# The seccomp launcher MUST be built by the normal build: with
	# the sandbox ON by default, a missing orion-seccomp makes
	# every shell_exec fail closed (F-A3-1), so a build that
	# forgets the launcher ships a broken agent. CGO_ENABLED=0
	# keeps it static, so it runs inside the chroot (see
	# internal/sandbox/seccomp.go).
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/orion-seccomp ./internal/sandbox/cmd/launcher

install:
	./install.sh

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	@test -z "$$(gofmt -l cmd internal)" || { echo "gofmt -l:"; gofmt -l cmd internal; exit 1; }

version:
	@echo "orion $(EFFECTIVE_VERSION) commit $(COMMIT) built $(DATE)"

clean:
	rm -rf $(BIN_DIR)
