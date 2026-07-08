# 安装指南

> 本指南面向 AI agent 和自动化部署。人类用户请参考 [README](README.md) 中的商店安装说明，可在 CPA WebUI 中一键安装。

## 1. 确认 CPA 运行时架构并下载成品

插件 `.so` 必须匹配 **CPA 容器或进程的运行时架构**，不是宿主机架构。先确认 CPA 实际跑在什么架构上：

```bash
# Docker 部署：查容器内架构（把 <service> 换成 docker-compose 里的服务名）
docker compose exec <service> uname -m

# 独立部署：直接查本机
uname -m
```

根据返回值选择对应的 zip：

```bash
set -euo pipefail

ARCH="<上面查到的输出>"
case "$ARCH" in
  x86_64|amd64)
    GOARCH="amd64"
    ;;
  aarch64|arm64)
    GOARCH="arm64"
    ;;
  *)
    echo "不支持的 CPU 架构：$ARCH。当前 release 只提供 Linux amd64/arm64 成品。" >&2
    exit 1
    ;;
esac

# 获取最新版本号（不带 v 前缀）
VERSION=$(curl -s https://api.github.com/repos/uf-hy/cpa-plugin-codexcomp/releases/latest \
  | python3 -c "import sys,json;print(json.load(sys.stdin)['tag_name'].lstrip('v'))")

ASSET="codexcomp_${VERSION}_linux_${GOARCH}.zip"
wget -qO "/tmp/${ASSET}" \
  "https://github.com/uf-hy/cpa-plugin-codexcomp/releases/latest/download/${ASSET}"

# 校验完整性
wget -qO /tmp/checksums.txt \
  "https://github.com/uf-hy/cpa-plugin-codexcomp/releases/latest/download/checksums.txt"
# 校验完整性：只校验目标文件那一行
cd /tmp && grep "  ${ASSET}$" /tmp/checksums.txt | sha256sum -c - || { echo "checksum verification failed"; exit 1; }

test -s "/tmp/${ASSET}"
```

## 2. 创建插件目录并解压

```bash
mkdir -p <CPA_DIR>/plugins
unzip -o "/tmp/${ASSET}" -d <CPA_DIR>/plugins/
```

## 3. 在 config.yaml 中启用插件

检查 `<CPA_DIR>/config.yaml`。如果没有 `plugins` 段，添加：

```yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    codexcomp:
      enabled: true
      priority: 1
      # 可选：自定义截断后的续写提示。不配置时默认是 Continue thinking...
      # marker_text: "Continue thinking..."
      # 可选：最多续写轮数。默认 3；设为 0 可临时禁用续写做 A/B 对比。
      # max_continue: 3
      # 可选：最大截断层级。默认 6；设为 0 表示不限制上限。
      # max_tier_n: 6
      # 可选：输出调试日志到 CPA host log。默认 false，排障时再开。
      # debug_log: false
```

如果已经有 `plugins` 段，确保 `plugins.enabled: true`，并确保 `configs.codexcomp.enabled: true` 存在。

## 4. 挂载插件目录（仅 Docker）

如果 CPA 跑在 Docker 里，确保 `docker-compose.yml` 有 plugins 卷映射：

```yaml
volumes:
  - ./plugins:/CLIProxyAPI/plugins
```

## 5. 重启 CPA

```bash
# Docker：
cd <CPA_DIR> && docker compose restart

# 独立部署：
systemctl restart cli-proxy-api
```

## 6. 验证

```bash
curl -sN <CPA_URL>/v1/responses \
  -H "Authorization: Bearer <YOUR_KEY>" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-5.5","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"Hi"}]}],"reasoning":{"effort":"low"}}' \
  | grep proxy_rounds
```

如果输出中看到 `proxy_rounds`，说明插件正常工作。

## 卸载

```bash
rm -f <CPA_DIR>/plugins/codexcomp*.so
cd <CPA_DIR> && docker compose restart
```

如果是独立部署，删除插件后改用对应的服务重启命令。

## 排障

- **插件没加载**：检查 CPA 日志中是否有 `codexcomp` 相关条目。确保 `plugins.enabled: true` 且 `.so` 文件在 `plugins` 目录中。
- **Docker 没挂载插件目录**：确认 `./plugins:/CLIProxyAPI/plugins` 已写入 `docker-compose.yml`，并且宿主机上的 `<CPA_DIR>/plugins/codexcomp.so` 存在。
- **架构不匹配**：`.so` 必须匹配 CPA 容器或进程的运行时架构，不是宿主机架构。Apple Silicon 上跑 Docker 需要确认容器实际是 `linux/amd64` 还是 `linux/arm64`。
- **想看续写是否触发**：临时设置 `debug_log: true`，重启 CPA 后查看 CPA 日志。最终响应里也会有 `metadata.proxy_rounds`、`metadata.proxy_billed_usage` 和可能的 `metadata.proxy_stopped_reason`。CPA 如果配置了 `debug: true` 和 `logging-to-file: true`，日志文件在容器内的 `/CLIProxyAPI/logs/main.log`。
