#!/usr/bin/env bash
# 输出 APP 备案 / 应用市场上架所需的三端「特征信息」：包名、公钥、证书指纹。
# 全部为公开信息，可直接抄进阿里云备案系统的「特征信息」表。
#
# 用法：
#   bash mobile/scripts/sign-fingerprints.sh [signing-dir]
#
# 约定（2026-11-05 定）：
#   - 安卓/iOS：密钥算法 RSA →「公钥」栏填 RSA 公钥模数（hex 512 字符）
#   - 鸿蒙：密钥算法 **EC secp256r1**（AGC 强制，RSA 的 CSR 会被判「无效」）
#           →「公钥」栏填 EC 公钥点 hex（04||X||Y，130 字符）
#   - 「证书MD5指纹」栏：安卓/鸿蒙 = 证书 MD5（32 位）；iOS = 证书 SHA-1（40 位，规范如此）
#   - 证书文件兼容 PEM 与 DER；AGC 下发的 .cer 可能是证书链，需取与私钥配对的那张叶子证书

set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SIGN="${1:-$ROOT/signing}"
APP_ID="com.ivec.ai"

cert_fp() { # $1=证书 $2=md5|sha1
  local f="$1" a="$2" v
  v=$(openssl x509 -in "$f" -noout -fingerprint "-$a" 2>/dev/null | awk -F= '{print $2}' | tr -d ':' | tr 'A-Z' 'a-z')
  if [ -z "$v" ]; then
    v=$(openssl x509 -in "$f" -inform DER -noout -fingerprint "-$a" 2>/dev/null | awk -F= '{print $2}' | tr -d ':' | tr 'A-Z' 'a-z')
  fi
  echo "$v"
}

cert_mod() { # $1=证书 → RSA 模数 hex
  local f="$1" v
  v=$(openssl x509 -in "$f" -noout -modulus 2>/dev/null | awk -F= '{print $2}')
  if [ -z "$v" ]; then
    v=$(openssl x509 -in "$f" -inform DER -noout -modulus 2>/dev/null | awk -F= '{print $2}')
  fi
  echo "$v"
}

cert_ecpub() { # $1=证书 → EC 公钥点 hex（04||X||Y）
  local f="$1" v
  v=$(openssl x509 -in "$f" -noout -pubkey 2>/dev/null | openssl ec -pubin -outform DER 2>/dev/null | tail -c 65 | xxd -p | tr -d '\n')
  if [ -z "$v" ]; then
    v=$(openssl x509 -in "$f" -inform DER -noout -pubkey 2>/dev/null | openssl ec -pubin -outform DER 2>/dev/null | tail -c 65 | xxd -p | tr -d '\n')
  fi
  echo "$v"
}

csr_ecpub() { # $1=csr → EC 公钥点 hex
  openssl req -in "$1" -noout -pubkey 2>/dev/null | openssl ec -pubin -outform DER 2>/dev/null | tail -c 65 | xxd -p | tr -d '\n'
}

# 从证书链里挑出与给定公钥配对的那张叶子证书（链文件顺序可能是 根→中间→叶子），复制到稳定路径后输出
leaf_match() { # $1=证书文件(可含链) $2=公钥 hex
  local f="$1" key="$2" tmpd c mk pk out
  tmpd=$(mktemp -d)
  out="${TMPDIR:-/tmp}/aic-harmony-leaf.pem"
  openssl crl2pkcs7 -nocrl -certfile "$f" 2>/dev/null | openssl pkcs7 -print_certs -out "$tmpd/all.pem" 2>/dev/null
  awk -v d="$tmpd" 'BEGIN{n=0} /BEGIN CERTIFICATE/{n++; f=sprintf("%s/c%d.pem", d, n)} {if(n>0) print > f}' "$tmpd/all.pem" 2>/dev/null
  for c in "$tmpd"/c*.pem; do
    [ -f "$c" ] || continue
    mk=$(openssl x509 -in "$c" -noout -modulus 2>/dev/null | awk -F= '{print $2}')
    if [ -n "$mk" ] && [ "$mk" = "$key" ]; then cp -f "$c" "$out"; rm -rf "$tmpd"; echo "$out"; return 0; fi
    pk=$(openssl x509 -in "$c" -noout -pubkey 2>/dev/null | openssl ec -pubin -outform DER 2>/dev/null | tail -c 65 | xxd -p | tr -d '\n')
    if [ -n "$pk" ] && [ "$pk" = "$key" ]; then cp -f "$c" "$out"; rm -rf "$tmpd"; echo "$out"; return 0; fi
  done
  rm -rf "$tmpd"
  return 1
}

echo "应用名: ivec AI"
echo "包名:   $APP_ID"
echo "签名目录: $SIGN"
echo

echo "=== Android (RSA 2048) ==="
ACER="$SIGN/android/ivec-release.cer"
if [ -f "$ACER" ]; then
  echo "包名:            $APP_ID"
  echo "公钥(模数hex):   $(cert_mod "$ACER")"
  echo "证书MD5指纹:     $(cert_fp "$ACER" md5)"
  echo "证书SHA1:        $(cert_fp "$ACER" sha1)"
  echo "keystore:        $SIGN/android/ivec-release.p12 (alias: ivec-release)"
else
  echo "[缺] $ACER —— 尚未生成 Android 密钥"
fi
echo

echo "=== iOS (RSA 2048) ==="
ICER="$SIGN/ios/AppleDistribution.cer"
if [ -f "$ICER" ]; then
  echo "Bundle ID:       $APP_ID"
  echo "公钥(模数hex):   $(cert_mod "$ICER")"
  echo "证书SHA1:        $(cert_fp "$ICER" sha1)   <- 填进「证书MD5指纹」栏"
  echo "证书MD5:         $(cert_fp "$ICER" md5)"
  echo "私钥p12:         $SIGN/ios/apple-distribution.p12"
else
  echo "[待办] 上传 $SIGN/ios/apple-distribution.csr 到 Apple 开发者门户申请 Apple Distribution 证书，"
  echo "       下载后另存为 $ICER（DER 格式即可），再重跑本脚本。"
fi
echo

echo "=== HarmonyOS (EC secp256r1) ==="
HCER="$SIGN/harmonyos/app-release.cer"
HCSR="$SIGN/harmonyos/app-release.standard-label.csr"
if [ -f "$HCER" ]; then
  KEY=""
  [ -f "$HCSR" ] && KEY=$(csr_ecpub "$HCSR")
  LEAF="$HCER"
  if [ -n "$KEY" ]; then
    M=$(leaf_match "$HCER" "$KEY")
    [ -n "$M" ] && LEAF="$M"
  fi
  echo "bundleName:      $APP_ID"
  echo "公钥(EC hex):    $(cert_ecpub "$LEAF")"
  echo "证书MD5指纹:     $(cert_fp "$LEAF" md5)"
  echo "证书SHA1:        $(cert_fp "$LEAF" sha1)"
  [ "$LEAF" != "$HCER" ] && echo "（已从证书链中匹配出叶子证书）"
else
  if [ -f "$HCSR" ]; then
    echo "[进行中] csr 已就绪（EC/P-256）："
    echo "       ① $SIGN/harmonyos/app-release.csr            ← keytool 原样输出"
    echo "       ② $SIGN/harmonyos/app-release.standard-label.csr ← 标签改成标准 CERTIFICATE REQUEST"
    echo "       ③ $SIGN/harmonyos/app-release.der.csr        ← 二进制 DER"
    echo "       → AGC 左侧「证书」→ 创建发布证书 → 上传 csr → 下载 .cer 放 $HCER"
    echo "       → 再创建发布 Profile（.p7b），最后重跑本脚本"
  else
    echo "[待办] ① AGC 建 APP ID（bundleName=$APP_ID，创建后不可改）"
    echo "       ② 生成 EC 密钥 + csr（keytool -keyalg EC -groupname secp256r1）"
    echo "       ③ AGC 证书管理上传 csr → 下载发布证书 .cer 放 $HCER"
    echo "       ④ AGC 创建发布 Profile（.p7b）→ 5. 重跑本脚本"
  fi
fi
echo

echo "提醒：以上指纹一旦提交 APP 备案就不能换签名密钥；换密钥 = 备案指纹失效，需走备案变更。"
