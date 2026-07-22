#!/usr/bin/env bash
# 一键发版: 交叉编译 -> 建 GitHub Release -> 更新并推送 homebrew-tap formula
# 用法: ./scripts/release.sh <version>   例: ./scripts/release.sh 0.2.0
set -euo pipefail

VERSION="${1:?用法: scripts/release.sh <version, 如 0.2.0>}"
REPO="TITOCHAN2023/claude-multi-acct"
TAP_DIR="${TAP_DIR:-$HOME/Project/homebrew-tap}"
FORMULA="$TAP_DIR/Formula/cc2.rb"
BASE="https://github.com/$REPO/releases/download/v$VERSION"

cd "$(dirname "$0")/.."

echo "==> 1/4 交叉编译 v$VERSION"
make release VERSION="$VERSION"

echo "==> 2/4 创建 GitHub Release v$VERSION"
gh release create "v$VERSION" dist/cc2-* dist/SHA256SUMS \
  --repo "$REPO" --target main --title "cc2 v$VERSION" \
  --notes "官网多账号并行/轮询工具。安装: brew install TITOCHAN2023/tap/cc2"

sha() { shasum -a 256 "dist/cc2-$1" | awk '{print $1}'; }
ARM_MAC=$(sha darwin-arm64); AMD_MAC=$(sha darwin-amd64)
ARM_LNX=$(sha linux-arm64);  AMD_LNX=$(sha linux-amd64)

echo "==> 3/4 更新 formula $FORMULA"
[ -d "$TAP_DIR" ] || { echo "找不到 tap 目录 $TAP_DIR (可用 TAP_DIR=... 覆盖)"; exit 1; }
cat > "$FORMULA" <<EOF
class Cc2 < Formula
  desc "Claude 官网多账号并行/轮询工具 (默认账号永远垫底)"
  homepage "https://github.com/$REPO"
  version "$VERSION"
  license "MIT"

  on_macos do
    on_arm do
      url "$BASE/cc2-darwin-arm64"
      sha256 "$ARM_MAC"
    end
    on_intel do
      url "$BASE/cc2-darwin-amd64"
      sha256 "$AMD_MAC"
    end
  end

  on_linux do
    on_arm do
      url "$BASE/cc2-linux-arm64"
      sha256 "$ARM_LNX"
    end
    on_intel do
      url "$BASE/cc2-linux-amd64"
      sha256 "$AMD_LNX"
    end
  end

  def install
    bin.install Dir["cc2-*"].first => "cc2"
  end

  test do
    assert_match "cc2", shell_output("#{bin}/cc2 help")
  end
end
EOF

echo "==> 4/4 推送 homebrew-tap"
git -C "$TAP_DIR" add -A
git -C "$TAP_DIR" commit -m "cc2 formula v$VERSION"
git -C "$TAP_DIR" push

echo "✅ 发版完成 v$VERSION —— 用户可 brew update && brew upgrade cc2"
