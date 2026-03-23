#!/bin/bash

set -e

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

cd "$(dirname "$0")"

USE_LOCAL=false
CLI_VERSION="${1:-latest}"
USE_PROXY="${2:-true}"

if [ "$CLI_VERSION" = "--local" ]; then
  USE_LOCAL=true
  CLI_VERSION="local"
fi

if [ "$CLI_VERSION" = "false" ] || [ "$CLI_VERSION" = "true" ]; then
  USE_PROXY="$CLI_VERSION"
  CLI_VERSION="latest"
fi

if [ -f .env ]; then
  echo -e "${GREEN}✅ 加载 .env 配置文件...${NC}"
  set -a
  source .env
  set +a
else
  echo -e "${YELLOW}⚠️  未找到 .env 文件，按环境变量继续${NC}"
fi

# 当前仓库默认镜像名，避免被外部项目 .env 的 IMAGE_NAME=api-proxy 误导
DEFAULT_IMAGE_NAME="cli-proxy-api-plus"
IMAGE_NAME="${CLIPROXY_IMAGE_NAME:-$DEFAULT_IMAGE_NAME}"
CONTAINER_NAME="${CONTAINER_NAME:-cliproxyapi-plus}"
PORT="${PORT:-8317}"

if [ "$USE_LOCAL" = false ]; then
  if [ -z "$GITHUB_USERNAME" ]; then
    echo -e "${RED}❌ 错误: 请设置 GITHUB_USERNAME${NC}"
    exit 1
  fi
  IMAGE_PREFIX="ghcr.io/${GITHUB_USERNAME}/${IMAGE_NAME}"
  FULL_IMAGE="${IMAGE_PREFIX}:${CLI_VERSION}"
else
  FULL_IMAGE="${IMAGE_NAME}:local"
fi

if ! command -v docker >/dev/null 2>&1; then
  echo -e "${RED}❌ Docker 未安装${NC}"
  exit 1
fi
if ! docker info >/dev/null 2>&1; then
  echo -e "${RED}❌ Docker 服务未运行${NC}"
  exit 1
fi

if [ "$USE_LOCAL" = false ] && [ -n "$GITHUB_TOKEN" ]; then
  echo -e "${BLUE}🔐 登录 ghcr.io...${NC}"
  echo "$GITHUB_TOKEN" | docker login ghcr.io -u "$GITHUB_USERNAME" --password-stdin
fi

if [ "$USE_LOCAL" = false ]; then
  echo -e "${BLUE}📥 拉取镜像: ${FULL_IMAGE}${NC}"
  docker pull "${FULL_IMAGE}"
else
  echo -e "${YELLOW}📍 本地镜像模式: ${FULL_IMAGE}${NC}"
  docker image inspect "${FULL_IMAGE}" >/dev/null
fi

echo -e "${BLUE}🛑 停止旧容器...${NC}"
docker rm -f "${CONTAINER_NAME}" 2>/dev/null || true

echo -e "${BLUE}🚀 启动新容器...${NC}"
docker run -d \
  --name "${CONTAINER_NAME}" \
  --network host \
  -v "/root/cliproxyapi-plus:/app" \
  --restart unless-stopped \
  "${FULL_IMAGE}"

echo -e "${GREEN}✅ 部署完成${NC}"
echo -e "${YELLOW}容器: ${CONTAINER_NAME}${NC}"
echo -e "${YELLOW}端口(配置内): ${PORT}${NC}"
echo -e "${YELLOW}查看日志: docker logs -f ${CONTAINER_NAME}${NC}"

