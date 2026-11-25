#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 2 ]]; then
  echo "usage: $0 <version> <checksums.txt>" >&2
  exit 1
fi

raw_version="$1"
checksums_file="$2"
repo="${MPLEX_RELEASE_REPO:-EricWF/mplex}"

if [[ -z "$raw_version" ]]; then
  echo "error: version must not be empty" >&2
  exit 1
fi

if [[ ! -r "$checksums_file" ]]; then
  echo "error: checksums file not readable: $checksums_file" >&2
  exit 1
fi

if [[ "$raw_version" == v* ]]; then
  tag="$raw_version"
  version="${raw_version#v}"
else
  tag="v${raw_version}"
  version="$raw_version"
fi

if [[ -z "$version" ]]; then
  echo "error: invalid version: $raw_version" >&2
  exit 1
fi

lookup_sha() {
  local asset="$1"
  local sha
  sha="$(awk -v target="$asset" '
    NF >= 2 {
      file = $2
      sub(/^\*/, "", file)
      sub(/^\.\//, "", file)
      sub(/^.*\//, "", file)
      if (file == target) {
        print tolower($1)
        exit
      }
    }
  ' "$checksums_file")"

  if [[ -z "$sha" ]]; then
    echo "error: checksum for ${asset} not found in ${checksums_file}" >&2
    exit 1
  fi
  if [[ ! "$sha" =~ ^[0-9a-f]{64}$ ]]; then
    echo "error: invalid sha256 for ${asset}: ${sha}" >&2
    exit 1
  fi
  printf '%s' "$sha"
}

sha_darwin_arm64="$(lookup_sha "mplex-darwin-arm64.tar.gz")"
sha_darwin_amd64="$(lookup_sha "mplex-darwin-amd64.tar.gz")"
sha_linux_arm64="$(lookup_sha "mplex-linux-arm64.tar.gz")"
sha_linux_amd64="$(lookup_sha "mplex-linux-amd64.tar.gz")"

base_url="https://github.com/${repo}/releases/download/${tag}"

cat <<EOF
class Mplex < Formula
  desc "Native-behavior terminal multiplexer with fzf-style session switcher"
  homepage "https://github.com/${repo}"
  version "${version}"
  license "MIT"

  on_macos do
    on_arm do
      url "${base_url}/mplex-darwin-arm64.tar.gz"
      sha256 "${sha_darwin_arm64}"
    end

    on_intel do
      url "${base_url}/mplex-darwin-amd64.tar.gz"
      sha256 "${sha_darwin_amd64}"
    end
  end

  on_linux do
    on_arm do
      url "${base_url}/mplex-linux-arm64.tar.gz"
      sha256 "${sha_linux_arm64}"
    end

    on_intel do
      url "${base_url}/mplex-linux-amd64.tar.gz"
      sha256 "${sha_linux_amd64}"
    end
  end

  def install
    bin.install "mplex"
    generate_completions_from_executable(bin/"mplex", "completion")
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/mplex --version")
  end
end
EOF
