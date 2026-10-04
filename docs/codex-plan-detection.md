# Codex plan detection

## Source of truth

The Codex usage endpoint returns current `plan_type` alongside `rate_limit`
windows. A login's ID-token plan can outlive the subscription. Switcher formerly
parsed but discarded the usage response's plan, retaining Pro after a downgrade.

The reproduced case returned `plan_type: free` with a zero-used monthly window,
while the stored account still said `pro`. The second account returned
`prolite`, matching its stored plan. These were authenticated read-only quota
checks, not inference requests or subscription mutations.

`provider.Usage.Plan` now carries a nonempty upstream plan through the same
successful usage result. The proxy saves it under the existing account lock
before publishing the corresponding quota. It preserves credentials, account
preferences and routing. An absent plan or failed quota request retains prior
metadata. Providers with separate plan lookups still use their existing path
when the usage result does not supply a plan.

Both UIs already map `free` to **Free**. The fix makes that label receive current
data rather than adding an inference from the duration or fullness of quota.

## Free plan and Luna

OpenAI's [Codex pricing documentation](https://developers.openai.com/codex/pricing)
currently lists GPT-6 Luna at Standard speed in the desktop app for Free and
Go plans, subject to rollout. This is product documentation, not a guarantee of
model availability for a particular account or CLI. Switcher displays the
reported subscription tier and quota; it does not infer model entitlements or
route requests to Luna based solely on `free`.
