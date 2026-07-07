# 安装指南（面向 AI Agent）

本指南为 AI agent（自动化代理，如 Codex、Claude Code 等）编写，用于自动安装 CodexComp CPA 插件。

遇到 `<CPA_DIR>`、`<CPA_URL>`、`<YOUR_KEY>` 这类占位符时，必须先向用户确认真实值，不要猜测路径、端口或密钥。

## 安装前（可选）：A/B 降智测试

如果当前机器已经 clone 了本仓库，可以询问用户是否要先跑一次 [codex-candy-eval](https://github.com/haowang02/codex-candy-eval) 降智测试，记录无插件时的基线表现。安装完成后再跑一次，对比插件效果。

```bash
python3 scripts/candy_eval_cpa.py --url <CPA_URL>/v1/responses --key <YOUR_KEY> -n 5 -r high
```

如果当前机器只有 CPA 部署目录，没有本仓库和 `scripts/candy_eval_cpa.py`，跳过此步骤。

## 前置条件

- 一个运行中的 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 实例（建议使用[最新 release](https://github.com/router-for-me/CLIProxyAPI/releases/latest)）
- CPA 部署目录的访问权限
- 可以修改 CPA 的 `config.yaml`
- Linux amd64 或 Linux arm64 运行环境

## 安装步骤

### 1. 确认 CPU 架构并下载成品

```bash
set -euo pipefail

ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64)
    ASSET="codexcomp-linux-amd64.so"
    ;;
  aarch64|arm64)
    ASSET="codexcomp-linux-arm64.so"
    ;;
  *)
    echo "不支持的 CPU 架构：$ARCH。当前 release 只提供 Linux amd64/arm64 成品。" >&2
    exit 1
    ;;
esac

wget -qO /tmp/codexcomp.so \
  "https://github.com/uf-hy/cpa-plugin-codexcomp/releases/latest/download/${ASSET}"

test -s /tmp/codexcomp.so
```

### 2. 创建插件目录

```bash
mkdir -p <CPA_DIR>/plugins
```

### 3. 安装插件文件

```bash
install -m 0644 /tmp/codexcomp.so <CPA_DIR>/plugins/codexcomp.so
```

### 4. 在 config.yaml 中启用插件

检查 `<CPA_DIR>/config.yaml`。如果没有 `plugins` 段，添加：

```yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    codexcomp:
      enabled: true
      priority: 1
```

如果已经有 `plugins` 段，确保 `plugins.enabled: true`，并确保 `configs.codexcomp.enabled: true` 存在。

### 5. 挂载插件目录（仅 Docker）

如果 CPA 跑在 Docker 里，确保 `docker-compose.yml` 有 plugins 卷映射：

```yaml
volumes:
  - ./plugins:/CLIProxyAPI/plugins:ro
```

### 6. 重启 CPA

```bash
# Docker：
cd <CPA_DIR> && docker compose restart

# 独立部署：
systemctl restart cli-proxy-api
```

### 7. 验证

通过 CPA 发一个简单的 gpt-5.5 流式请求。如果插件已加载并接管请求，最终 `response.completed` 事件会包含 `metadata.proxy_rounds`：

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
rm <CPA_DIR>/plugins/codexcomp.so
cd <CPA_DIR> && docker compose restart
```

如果是独立部署，删除插件后改用对应的服务重启命令。

## 排障

- **插件没加载**：检查 CPA 日志中是否有 `codexcomp` 相关条目。确保 `plugins.enabled: true` 且 `.so` 文件在 `plugins` 目录中。
- **Docker 没挂载插件目录**：确认 `./plugins:/CLIProxyAPI/plugins:ro` 已写入 `docker-compose.yml`，并且宿主机上的 `<CPA_DIR>/plugins/codexcomp.so` 存在。
- **架构不匹配**：`.so` 必须匹配 CPA 容器或进程的运行时架构，不是宿主机架构。Apple Silicon 上跑 Docker 需要确认容器实际是 `linux/amd64` 还是 `linux/arm64`。
