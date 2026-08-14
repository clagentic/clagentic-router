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
// The AWS SDK credential vars are listed here as literals too, not a
// prefix+denylist. A suffix-based denylist (blocking names ending in
// _TOKEN/_SECRET) was evaluated and rejected: the AWS SDK's own standard
// credential vars include AWS_SESSION_TOKEN and AWS_SECRET_ACCESS_KEY — a
// denylist wide enough to catch operator-typo'd secrets would also block
// the exact vars this task exists to admit. The AWS SDK's env-var
// vocabulary is small, stable, and documented, so literal enumeration is
// both safer and no more maintenance burden than a prefix would be.
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
// # Why AWS is a flat literal list, and why Google/Azure are NOT treated the same
//
// The AWS set fixes a live, documented regression: Bedrock-fronted codex_cli
// (model_provider = amazon-bedrock in ~/.codex/config.toml) genuinely reads
// these vars today, and codex_cli's env was previously unfiltered, so
// filtering it without this set would have broken a working deployment
// (lr-bd5dc0). That is not true of Azure: no adapter in this repo talks to
// Azure OpenAI / Azure Identity, so there is no live regression and no
// deployment this admits. Pre-admitting a cloud provider's credential vars
// ahead of any adapter that reads them is not "breadth" in the sense
// CLAUDE.md's doctrine means — breadth there means naming what a change
// does on every existing provider, including the no-op case, not
// pre-opening a secret-admission boundary for a provider with no consumer.
// AZURE_TENANT_ID, AZURE_CLIENT_ID, AZURE_CLIENT_SECRET,
// AZURE_CLIENT_CERTIFICATE_PATH, AZURE_USERNAME, and AZURE_PASSWORD
// (lr-268431, bobbie.uncat.1) were removed in full rather than partially
// kept, because unlike the Google set below, every var in Azure's SDK
// credential-chain vocabulary that would be worth admitting either IS raw
// secret material (AZURE_CLIENT_SECRET, AZURE_PASSWORD) or exists only to
// pair with one (AZURE_TENANT_ID/AZURE_CLIENT_ID/AZURE_USERNAME identify an
// auth flow that has no other purpose once its secret half is gone;
// AZURE_CLIENT_CERTIFICATE_PATH is a path but authenticates via a
// certificate this repo has no story for provisioning either). Keeping a
// stub of identity-only vars with no working auth flow behind them would
// just be dead surface. The clean path for a future Azure adapter: add the
// specific vars that adapter's SDK path needs, in the same PR that adds the
// adapter, the same way lr-bd5dc0 added AWS_* alongside the Bedrock fix
// this file documents above — not ahead of time.
//
// The Google/CLOUDSDK set is kept, on the opposite basis BOBBIE accepted:
// GOOGLE_APPLICATION_CREDENTIALS and CLOUDSDK_CONFIG are file paths,
// GOOGLE_CLOUD_PROJECT and CLOUDSDK_CORE_PROJECT are project identifiers —
// none is raw secret material, and gemini_cli is a live adapter in this
// repo today (unlike Azure, which has no adapter at all). The exposure of
// an unused project-id or config-path var is qualitatively smaller than the
// exposure of an unused long-lived secret.
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
// the same allowlist). A flat list is the deliberately chosen, smaller
// alternative for the cloud-provider families it does cover. Per-adapter/
// per-model scoping remains a real hardening option if the flat list proves
// too broad in practice; it is not implemented here.
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

	// Google Cloud SDK / Vertex AI credential env vars. gemini_cli is a live
	// adapter in this repo, and none of these four is raw secret material —
	// GOOGLE_APPLICATION_CREDENTIALS/CLOUDSDK_CONFIG are file paths,
	// GOOGLE_CLOUD_PROJECT/CLOUDSDK_CORE_PROJECT are project identifiers.
	// See package doc for why this set is kept while the Azure set
	// (secret-shaped, no adapter consumer) was removed (lr-268431).
	"GOOGLE_APPLICATION_CREDENTIALS",
	"GOOGLE_CLOUD_PROJECT",
	"CLOUDSDK_CORE_PROJECT",
	"CLOUDSDK_CONFIG",
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
