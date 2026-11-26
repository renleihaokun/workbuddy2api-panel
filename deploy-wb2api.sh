#!/usr/bin/env bash
# deploy-wb2api.sh — 单文件二进制的滚动更新（生产机在仓库根目录执行）。
#
# 必填：EXPECT=<新二进制 sha256>  VER=<appVersion，例如 1.27.1-panel>
# 可选：NEW=./wb2api.new  BIN=./wb2api  SERVICE=workbuddy-panel
#       HEALTH_URL=http://127.0.0.1:7863/healthz  HEALTH_TIMEOUT=30  DRY_RUN=1
#
# DRY_RUN=1 只校验 sha256 / 版本串，不动文件、不重启服务。
set -Eeuo pipefail

log() { printf '[%s] %s\n' "$(date '+%F %T')" "$*" | tee -a "$LOG"; }
finish() { printf '%s\n' "$1" >"$STATUS_FILE"; }
on_error() {
  local rc=$?
  finish "FAILED rc=${rc} at $(date '+%F %T') log=${LOG}"
  printf '[%s] 部署失败（rc=%s），日志：%s\n' "$(date '+%F %T')" "$rc" "$LOG" >&2
  exit "$rc"
}

cd -- "$(dirname -- "$0")"
STAMP="$(date +%Y%m%d-%H%M%S)"
LOG="deploy-${STAMP}.log"
STATUS_FILE="deploy-last-status"
trap on_error ERR

EXPECT="${EXPECT:-}"
VER="${VER:-}"
NEW="${NEW:-./wb2api.new}"
BIN="${BIN:-./wb2api}"
SERVICE="${SERVICE:-workbuddy-panel}"
HEALTH_URL="${HEALTH_URL:-http://127.0.0.1:7863/healthz}"
HEALTH_TIMEOUT="${HEALTH_TIMEOUT:-30}"
DRY_RUN="${DRY_RUN:-0}"

require_cmd() { command -v "$1" >/dev/null 2>&1 || { log "缺少命令：$1"; exit 1; }; }
[[ -n "$EXPECT" ]] || { log '必须设置 EXPECT=<新二进制 sha256>'; exit 1; }
[[ -n "$VER" ]] || { log '必须设置 VER=<新二进制 appVersion>'; exit 1; }
[[ -f "$NEW" ]] || { log "找不到新二进制：$NEW"; exit 1; }

require_cmd sha256sum
ACTUAL="$(sha256sum "$NEW" | awk '{print $1}')"
[[ "$ACTUAL" == "$EXPECT" ]] || { log "sha256 不匹配：期望 ${EXPECT}，实际 ${ACTUAL}"; exit 1; }
# 直接扫二进制，不要用 `strings | grep -q`：脚本开了 pipefail，grep -q 命中后
# 提前退出会让 strings 收到 SIGPIPE（141），整条管道被误判成失败。
grep -aFq -- "$VER" "$NEW" || { log "新二进制中找不到版本串：${VER}"; exit 1; }
log "校验通过：sha256=${ACTUAL} version=${VER}"

if [[ "$DRY_RUN" == "1" ]]; then
  finish "DRY_RUN_OK version=${VER} at $(date '+%F %T') log=${LOG}"
  exit 0
fi

[[ -f "$BIN" ]] || { log "找不到当前二进制：$BIN"; exit 1; }
require_cmd systemctl
require_cmd curl
require_cmd install

BACKUP="${BIN}.bak-${STAMP}"
TMP="${BIN}.new-${STAMP}"
install -m 0755 "$BIN" "$BACKUP"
install -m 0755 "$NEW" "$TMP"
log "已备份 ${BIN} → ${BACKUP}"

rollback() {
  log "开始回滚到 ${BACKUP}"
  install -m 0755 "$BACKUP" "$TMP"
  mv -f "$TMP" "$BIN"
  systemctl restart "$SERVICE" || true
  if curl -fsS --max-time 5 "$HEALTH_URL" >/dev/null 2>&1; then
    log "回滚后服务已恢复"
    finish "ROLLED_BACK at $(date '+%F %T') log=${LOG}"
  else
    log "回滚后健康检查仍失败，请人工介入"
    finish "ROLLBACK_FAILED at $(date '+%F %T') log=${LOG}"
  fi
  exit 1
}

log "原子替换 ${BIN}"
mv -f "$TMP" "$BIN"
log "重启 ${SERVICE}"
systemctl restart "$SERVICE" || rollback

log "健康检查 ${HEALTH_URL}（最多 ${HEALTH_TIMEOUT}s）"
deadline=$(( $(date +%s) + HEALTH_TIMEOUT ))
until curl -fsS --max-time 5 "$HEALTH_URL" >/dev/null 2>&1; do
  (( $(date +%s) >= deadline )) && { log "健康检查超时"; rollback; }
  sleep 2
done
log "部署成功：${VER}"
finish "SUCCESS version=${VER} at $(date '+%F %T') log=${LOG}"