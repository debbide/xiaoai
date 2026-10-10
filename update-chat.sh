#!/bin/bash
# 一键更新 chat 服务端
set -e
TAG=${1:-v1.0.0}
URL="https://github.com/debbide/xiaoai/releases/download/${TAG}/chat_Linux_x86_64.tar.gz"
echo "下载 $URL ..."
cd /tmp
curl -sLO "$URL"
tar xzf chat_Linux_x86_64.tar.gz
echo "停止旧服务..."
pkill -x chat || true
sleep 2
# 如果还没死，强制杀
pkill -9 -x chat || true
sleep 1
echo "替换二进制..."
cp -f chat /opt/open-xiaoai-chat/chat
chmod +x /opt/open-xiaoai-chat/chat
echo "启动新服务..."
cd /opt/open-xiaoai-chat
nohup ./chat > /tmp/chat.log 2>&1 &
echo "完成，PID: $!"
