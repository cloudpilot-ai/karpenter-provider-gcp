# Reporting Security Issues

If you discover a potential security issue in this project, the preferred reporting channel is [GitHub Private Vulnerability Reporting](https://github.com/cloudpilot-ai/karpenter-provider-gcp/security/advisories/new) (Security → Advisories → Report a vulnerability). Reports are private, not public issues.

If you cannot use GitHub reporting, email [security@cloudpilot.ai](mailto:security@cloudpilot.ai) or contact a [maintainer](MAINTAINERS.md) privately using an email address published on their linked GitHub profile, if available.

Please do not report undisclosed vulnerabilities in public issues, pull requests, discussions, or GitHub mentions, or include credentials, kubeconfigs, service-account keys, or unrelated personal data.

Include affected versions, impact, and reproduction steps or a minimal proof of concept, with sensitive infrastructure identifiers redacted.

## Response ownership and handling

The [maintainers](MAINTAINERS.md) collectively serve as the security response team under [governance](GOVERNANCE.md). Maintainers must designate willing responders and verify their access to private GitHub reports and the fallback mailbox, along with their notification settings. A listing in `OWNERS` or `MAINTAINERS.md` does not itself grant access or ensure notifications. Confirm a private escalation route if the reporting channels are unavailable or a responder is implicated.

The team's responsibilities are to:

1. Acknowledge and privately triage reports, assess affected versions and impact, and coordinate with the reporter.
2. Limit report access to responders who need it; exclude implicated responders and arrange an independent handler where necessary.
3. Coordinate remediation, tests, and release preparation privately, involving affected upstream projects when appropriate.
4. Agree a disclosure plan with the reporter and affected parties, publish fixes and an advisory with affected/fixed versions and mitigations, and credit the reporter with their consent.
5. Review response ownership and access at least annually and on role changes.

Do not use public issue escalation for an undisclosed vulnerability.
