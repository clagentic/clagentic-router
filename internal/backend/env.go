// internal/backend/env.go — filtered environment construction for CLI subprocess adapters.
//
// CLI adapters (claude_cli, codex_cli, codex_subagent, gemini_cli) inherit only
// a curated subset of the daemon's environment. This prevents API keys, router
// tokens, and deployment secrets from leaking into subprocess environments. (lr-c7ac)
//
// # Membership is expressed as literals, plus a small structural prefix set
//
// cliEnvAllowlistLiterals is an exact-match set: every operator-set var name
// a CLI or a backend's own cloud-provider SDK genuinely reads. Exact match
// is the default because it cannot admit a secret-shaped var that merely
// shares a stem — CODEX_API_KEY, CLAUDE_API_KEY, and GEMINI_API_KEY are
// exactly that shape and must never pass this filter (this repo treats
// provider API keys as router-level secrets referenced via router.yaml's
// env:VAR indirection, never inherited raw by a CLI subprocess — see
// .env.example).
//
// The cloud-provider SDK credential vars (AWS_*, GOOGLE_*/CLOUDSDK_*,
// AZURE_*) are listed here as literals too, not a prefix+denylist. A
// suffix-based denylist (blocking names ending in _TOKEN/_SECRET) was
// evaluated and rejected: the AWS SDK's own standard credential vars
// include AWS_SESSION_TOKEN and AWS_SECRET_ACCESS_KEY — a denylist wide
// enough to catch operator-typo'd secrets would also block the exact vars
// this task exists to admit. Each cloud SDK's env-var vocabulary is small,
// stable, and documented (AWS SDK, Google Application Default Credentials,
// Azure Identity SDK), so literal enumeration is both safer and no more
// maintenance burden than a prefix would be.
//
// cliEnvAllowlistPrefixes is reserved for genuinely unbounded families
// (locale/XDG vars) where no finite literal list is possible.
//
// # Why cloud-provider vars are structurally different from a bare CLI prefix
//
// A backend's own cloud-provider SDK credentials are a different category
// from router bearer tokens and other backends' API keys — they are the
// SDK-standard credential chain a cloud-fronted backend's OWN auth path
// depends on, not something another backend or the daemon itself would
// leak. Listing them does not reopen the lr-c7ac leak the same way a bare
// CLAUDE_/CODEX_/GEMINI_ prefix did.
//
// # Why a flat cloud-provider list, not per-adapter/per-model routing
//
// The router's BackendConfig has no notion of "this backend's upstream
// cloud provider" independent of which CLI/adapter it names — a codex_cli
// backend can be ChatGPT-Plus OAuth or Bedrock-fronted depending on the
// operator's local ~/.codex/config.toml, not anything buildCLIEnv can see.
// Plumbing that awareness into env.go would require threading provider
// identity from config through adapter construction into every buildCLIEnv
// call site — a much larger change than this defect calls for, and env.go
// has no other per-adapter conditional logic today (every call site uses
// the same allowlist). A flat list covering the three known cloud-provider
// SDK credential families is the deliberately chosen, smaller alternative:
// it fixes the live Bedrock regression and pre-empts the identical Vertex/
// Azure-shaped gap without new plumbing. The trade-off: a
// CLI that has no business talking to, say, Azure still gets AZURE_* vars
// if the operator's shell happens to export them — acceptable because (a)
// these are the backend's own upstream-cloud credentials, not another
// backend's secret or a router token, and (b) an unused credential var
// passed to a CLI that ignores it is a strictly smaller exposure than the
// AWS_-absence live regression this task fixes. Per-adapter/per-model
// scoping remains a real hardening option if the flat list proves too
// broad in practice; it is not implemented here.
package backend

import (
	"os"
	"strings"
)

// cliEnvAllowlistLiterals is the set of exact env var names passed to CLI
// subprocess adapters. See package doc for why literals are preferred over
// a prefix wherever the real requirement is enumerable.
var cliEnvAllowlistLiterals = []string{
	"PATH",
	"HOME",
	"USER",
	"SHELL",
	"TERM",
	"LANG",
	"TMPDIR",
	"TMP",
	"TEMP",

	// claude CLI: binary override (binpath.go) and CLI-native config dir.
	"CLAUDE_BIN",
	"CLAUDE_CONFIG_DIR",

	// codex CLI: binary override (binpath.go) and auth/config home
	// (codex_discovery.go's codexConfigPath, ~/.codex/config.toml and
	// auth.json resolution for both OAuth-session and Bedrock-env auth).
	"CODEX_BIN",
	"CODEX_HOME",

	// gemini CLI: binary override (binpath.go) only. GEMINI_API_KEY is
	// deliberately NOT here — it is the CLI's own documented API-key auth
	// path (see gemini_cli.go package doc) but is secret-shaped exactly
	// like CODEX_API_KEY/CLAUDE_API_KEY; an operator who wants gemini_cli
	// to use it sets it via router.yaml's extra-env mechanism, not by
	// relying on daemon-environment inheritance.
	"GEMINI_BIN",

	// Clagentic session vars that adapters intentionally propagate.
	"CLAGENTIC_DISABLE_RECALL",
	"CLAGENTIC_CODEX_TIER",

	// AWS SDK standard credential/config env vars (Bedrock-fronted CLI
	// backends, e.g. codex_cli / codex_subagent pointed at model_provider =
	// amazon-bedrock in ~/.codex/config.toml). bedrock_api is an HTTP
	// adapter with no subprocess and never goes through this filter — its
	// own config.LoadDefaultConfig call reads these directly from the
	// daemon's real environment, unaffected by this list either way.
	// codex_cli had never been filtered through buildCLIEnv before
	// lr-bd5dc0, so this family's absence was invisible until that change
	// routed a Bedrock-env-authed host through the filter for the first
	// time. Set matches the AWS SDK's documented env-credential-chain vars
	// (https://docs.aws.amazon.com/sdkref/latest/guide/settings-reference.html).
	"AWS_PROFILE",
	"AWS_REGION",
	"AWS_DEFAULT_REGION",
	"AWS_ACCESS_KEY_ID",
	"AWS_SECRET_ACCESS_KEY",
	"AWS_SESSION_TOKEN",
	"AWS_ROLE_ARN",
	"AWS_WEB_IDENTITY_TOKEN_FILE",
	"AWS_SDK_LOAD_CONFIG",
	"AWS_CONFIG_FILE",
	"AWS_SHARED_CREDENTIALS_FILE",

	// Google Cloud SDK / Vertex AI standard credential env vars. Not known
	// to be in live use by any adapter in this repo today, but gemini_cli
	// (or any future Vertex-fronted CLI backend) would hit the identical
	// gap AWS_ absence caused for Bedrock the moment such a backend
	// existed — pre-empted here per CLAUDE.md breadth doctrine rather than
	// waiting for its own live regression.
	"GOOGLE_APPLICATION_CREDENTIALS",
	"GOOGLE_CLOUD_PROJECT",
	"CLOUDSDK_CORE_PROJECT",
	"CLOUDSDK_CONFIG",

	// Azure OpenAI / Azure Identity SDK standard credential env vars. Same
	// rationale as the Google set above — no adapter in this repo talks to
	// Azure OpenAI today, but the SDK-standard env-credential-chain shape
	// is identical.
	"AZURE_TENANT_ID",
	"AZURE_CLIENT_ID",
	"AZURE_CLIENT_SECRET",
	"AZURE_CLIENT_CERTIFICATE_PATH",
	"AZURE_USERNAME",
	"AZURE_PASSWORD",
}

// cliEnvAllowlistPrefixes is reserved for genuinely unbounded families where
// no finite literal list is possible (locale/XDG base-directory vars have
// operator-defined suffixes, e.g. LC_ALL, LC_TIME, XDG_CONFIG_HOME).
// Cloud-provider SDK credentials are deliberately NOT expressed here — see
// package doc for why a literal list is used for those instead.
var cliEnvAllowlistPrefixes = []string{
	"LC_",
	"XDG_",
}

// buildCLIEnv constructs a filtered environment for CLI subprocess adapters.
// Only variables matching cliEnvAllowlistLiterals or cliEnvAllowlistPrefixes
// are inherited from the daemon. extra is appended last and takes
// precedence — any key that appears in extra is excluded from the daemon
// environment to prevent duplicate/shadowed entries.
func buildCLIEnv(extra []string) []string {
	// Build set of keys overridden by extra so we can drop them from daemon env.
	override := make(map[string]struct{}, len(extra))
	for _, kv := range extra {
		key := kv
		if idx := strings.IndexByte(kv, '='); idx >= 0 {
			key = kv[:idx]
		}
		override[key] = struct{}{}
	}

	var env []string
	for _, kv := range os.Environ() {
		key := kv
		if idx := strings.IndexByte(kv, '='); idx >= 0 {
			key = kv[:idx]
		}
		if _, overridden := override[key]; overridden {
			continue // extra wins
		}
		if cliEnvAllowed(kv) {
			env = append(env, kv)
		}
	}
	return append(env, extra...)
}

// cliEnvAllowed reports whether kv's key is admitted by the allowlist:
// an exact match against cliEnvAllowlistLiterals, or a prefix match against
// cliEnvAllowlistPrefixes.
func cliEnvAllowed(kv string) bool {
	key := kv
	if idx := strings.IndexByte(kv, '='); idx >= 0 {
		key = kv[:idx]
	}

	for _, literal := range cliEnvAllowlistLiterals {
		if key == literal {
			return true
		}
	}

	for _, prefix := range cliEnvAllowlistPrefixes {
		if key == prefix || strings.HasPrefix(key, prefix) {
			return true
		}
	}

	return false
}
