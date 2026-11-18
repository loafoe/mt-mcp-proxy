// Global configuration for the self-hosted Renovate action. Repository
// configuration (extends, packageRules) lives in renovate.json at the root: a
// custom configurationFile is read as global config, so it is not the place
// for repository rules.
//
// There is deliberately no `username`. It is the account Renovate runs as,
// and it is what Renovate searches for when it looks for its own issues and
// pull requests - the dependency dashboard among them. Hardcoding a bot name
// would be wrong here: without a RENOVATE_TOKEN the action runs as
// github-actions[bot], and a mismatched username means Renovate never finds
// the dashboard it created, so it opens a new one on every run. Left unset,
// Renovate detects the account from the token in use.
module.exports = {
  gitAuthor: "Renovate Bot <bot@renovateapp.com>",
  onboarding: false,
  platform: "github",
  forkProcessing: "disabled",
};
