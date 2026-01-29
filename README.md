
# 简介

这是一个基于 Websocket & Fastapi 的超轻量级快速搭建 RPC 服务的 Template，用于快速在浏览器等 支持 WebSocket 的客户端解决RPC 调用问题

# 快速搭建步骤

1. 启动 python rpc_server_full.py # 你可以自行修改端口，包括Websocket 服务端端口和 API http 的端口
默认端口：ws：8765  http：8000

2. 将 服务端 JS 代码 rpc_client_tmp.js 中的 RPCClient 注入到目标客户端中。底部有使用示例供给参考。

3. 然后在目标客户端注入成功后，你可以通过 http 或者 websocket 两种方式实现 RPC 远程调用。

提供接口：

http://127.0.0.1:8000/rpc/list # 查询注册的接口

http://127.0.0.1:8000/rpc/call # 调用接口


http_call_demo.py  为 http 调用示例
websocket_call_demo.py 为 websocket 调用示例

以下为注入示例
rpc_client_tmp.js
temp_client.html
temp_client2.html


# 如果你不想使用 python，你还可以通过 我编译好的 rpc_server.exe 可执行文件直接构建 服务端 (Window 端)
其他端你可以自行编译，或者参照 python 源码 或者 golang 源码进行重构。


rpc_server_go 文件夹这中 的 rpc_server.exe 文件 就是 go构建的服务端，如果你不需要修改端口 直接双击即可使用

如果你想要修改端口 可以通过 命令行启动，预留参数可以让你修改 websocket 或者 http 服务的端口。

-ws-port 9000 -http-port 9001

### 如果你想要在其他端使用注入 RPC 客户端可以参考 JavaScript 客户端代码进行重构。


