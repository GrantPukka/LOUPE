# Homebrew

`brew install GrantPukka/tap/loupe` resolves to a **separate repository**,
`github.com/GrantPukka/homebrew-tap`, holding `Formula/loupe.rb`. Homebrew
requires the `homebrew-` name prefix; the tap cannot live in this repo.

## Cutting a release

The release workflow renders the formula and attaches it to the GitHub release
as `loupe.rb`. To publish:

1. Tag and push: `git tag v1.2.3 && git push origin v1.2.3`
2. Wait for the `release` workflow, then review the draft release
3. Download `loupe.rb` from it
4. Commit it to the tap repo as `Formula/loupe.rb`
5. Publish the draft release

## Why the formula is not checked in here

It carries the sha256 of four archives that do not exist until the release is
built. A checked-in formula would be wrong between every release and correct
for the few minutes after each one, and the failure mode is a broken install
for users rather than a red build for us.

`render-formula.sh` is the generator, and it refuses to emit a formula with a
missing hash.

## Creating the tap, once

```bash
gh repo create GrantPukka/homebrew-tap --public
git clone https://github.com/GrantPukka/homebrew-tap && cd homebrew-tap
mkdir -p Formula
# add Formula/loupe.rb from the release, commit, push
```

Then `brew install GrantPukka/tap/loupe` works with no further setup — Homebrew
maps `GrantPukka/tap` to `GrantPukka/homebrew-tap` automatically.

## Testing a formula before publishing

```bash
brew install --build-from-source ./Formula/loupe.rb
brew test loupe
brew audit --strict --online loupe
```

The formula's `test do` block does more than check the binary starts: it writes
two JSON lines, filters for the error, and asserts the other line is absent. A
formula whose test only runs `--version` passes while the tool is broken.
