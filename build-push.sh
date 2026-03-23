#!/bin/bash

set -e

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

CLI_VERSION="${1:-latest}"
USE_PROXY="${2:-true}"

if [ "$CLI_VERSION" = "false" ] || [ "$CLI_VERSION" = "true" ]; then
  USE_PROXY="$CLI_VERSION"
  CLI_VERSION="latest"
fi

cd "$(dirname "$0")"

if [ -f .env ]; then
  echo -e "${GREEN}✅ 加载 .env 配置文件...${NC}"
  set -a
  source .env
  set +a
else
  echo -e "${YELLOW}⚠️  未找到 .env 文件，使用环境变量${NC}"
fi

if [ -z "$GITHUB_USERNAME" ]; then
  echo -e "${RED}❌ 错误: 请在 .env 或环境变量中设置 GITHUB_USERNAME${NC}"
  exit 1
fi

if [ -z "$GITHUB_TOKEN" ]; then
  echo -e "${RED}❌ 错误: 请在 .env 或环境变量中设置 GITHUB_TOKEN${NC}"
  exit 1
fi

# 当前仓库默认镜像名，避免被外部项目 .env 的 IMAGE_NAME=api-proxy 误导
DEFAULT_IMAGE_NAME="cli-proxy-api-plus"
IMAGE_NAME="${CLIPROXY_IMAGE_NAME:-$DEFAULT_IMAGE_NAME}"
IMAGE_PREFIX="ghcr.io/${GITHUB_USERNAME}/${IMAGE_NAME}"
VERSION="${CLI_VERSION}"
FULL_IMAGE="${IMAGE_PREFIX}:${VERSION}"

ARCH=$(uname -m)
case $ARCH in
  x86_64) PLATFORM="amd64" ;;
  aarch64|arm64) PLATFORM="arm64" ;;
  *)
    echo -e "${RED}❌ 不支持的架构: $ARCH${NC}"
    exit 1
    ;;
esac

echo -e "${BLUE}==========================================${NC}"
echo -e "${BLUE}构建并推送 CLIProxyAPI 镜像${NC}"
echo -e "${BLUE}==========================================${NC}"
echo -e "${YELLOW}📦 镜像: ${FULL_IMAGE}${NC}"
echo -e "${YELLOW}🖥️  架构: ${PLATFORM}${NC}"

if ! command -v docker >/dev/null 2>&1; then
  echo -e "${RED}❌ Docker 未安装${NC}"
  exit 1
fi
if ! docker info >/dev/null 2>&1; then
  echo -e "${RED}❌ Docker 服务未运行${NC}"
  exit 1
fi

echo -e "${BLUE}🔐 登录 ghcr.io...${NC}"
echo "$GITHUB_TOKEN" | docker login ghcr.io -u "$GITHUB_USERNAME" --password-stdin

BUILD_ARGS=""
NETWORK_ARG=""
if [ "$USE_PROXY" = "true" ]; then
  PROXY_SOURCE="${DOCKER_PROXY:-$http_proxy}"
  if [ -n "$PROXY_SOURCE" ]; then
    NETWORK_ARG="--network=host"
    DOCKER_HTTP_PROXY=$(echo "$PROXY_SOURCE" | sed 's/host\.docker\.internal/127.0.0.1/g')
    DOCKER_HTTPS_PROXY=$(echo "${https_proxy:-$PROXY_SOURCE}" | sed 's/host\.docker\.internal/127.0.0.1/g')
    BUILD_ARGS="$BUILD_ARGS --build-arg http_proxy=$DOCKER_HTTP_PROXY"
    BUILD_ARGS="$BUILD_ARGS --build-arg https_proxy=$DOCKER_HTTPS_PROXY"
    BUILD_ARGS="$BUILD_ARGS --build-arg HTTP_PROXY=$DOCKER_HTTP_PROXY"
    BUILD_ARGS="$BUILD_ARGS --build-arg HTTPS_PROXY=$DOCKER_HTTPS_PROXY"
    echo -e "${YELLOW}🌐 使用代理: $DOCKER_HTTP_PROXY${NC}"
  fi
else
  echo -e "${YELLOW}🚫 不使用代理${NC}"
fi

echo -e "${BLUE}🔨 构建镜像...${NC}"
docker build $NETWORK_ARG $BUILD_ARGS -t "${FULL_IMAGE}" .

echo -e "${BLUE}📤 推送镜像...${NC}"
docker push "${FULL_IMAGE}"

if [ "$VERSION" != "latest" ]; then
  LATEST_IMAGE="${IMAGE_PREFIX}:latest"
  echo -e "${BLUE}🏷️  同步 latest 标签...${NC}"
  docker tag "${FULL_IMAGE}" "${LATEST_IMAGE}"
  docker push "${LATEST_IMAGE}"
fi

echo -e "${GREEN}✅ 完成: ${FULL_IMAGE}${NC}"

