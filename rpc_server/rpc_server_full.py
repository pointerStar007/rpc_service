#!/usr/bin/env python
# -*- coding: utf-8 -*-
# @Time    : 2026/1/27 21:02
# @Author  : Pointer
# @File    : rpc_server_full.py
# @Software: PyCharm

import json
import asyncio
import websockets
import sys
import uuid
import random  # 新增：用于随机选择客户端
from typing import Dict, Set, Any, Optional, Callable, Awaitable
from websockets.server import WebSocketServerProtocol
from fastapi import FastAPI, HTTPException, Body
from pydantic import BaseModel
import uvicorn

# ===================== 核心数据结构 =====================
INTERFACE_REGISTRY: Dict[str, Dict[str, Set[WebSocketServerProtocol]]] = {}
CLIENT_META: Dict[WebSocketServerProtocol, Dict[str, Any]] = {}
CALL_CONTEXT: Dict[str, asyncio.Future] = {}
app = FastAPI(title="RPC HTTP接口服务", version="1.0")


# ===================== 数据模型（FastAPI） =====================
class RPCCallRequest(BaseModel):
    group: str
    interface: str
    params: Dict[str, Any] = {}


# ===================== 核心工具函数（新增随机负载均衡） =====================
async def _invoke_interface(group: str, interface: str, params: Dict[str, Any]) -> Dict[str, Any]:
    """
    内部核心调用函数（支持随机选择客户端）
    :param group: 分组名
    :param interface: 接口名
    :param params: 调用参数
    :return: 客户端执行结果
    """
    # 1. 校验接口是否存在
    if group not in INTERFACE_REGISTRY or interface not in INTERFACE_REGISTRY[group]:
        return {"success": False, "error": f"接口 {group}.{interface} 未注册", "call_id": ""}

    target_clients = INTERFACE_REGISTRY[group][interface]
    if not target_clients:
        return {"success": False, "error": f"接口 {group}.{interface} 暂无可用客户端", "call_id": ""}

    # 2. 核心改造：随机选择一个客户端（替代固定选第一个）
    # 转换为列表，方便随机选择
    client_list = list(target_clients)
    # 随机抽取一个客户端
    target_client = random.choice(client_list)  # 关键修改：随机选择
    print(f"负载均衡 - 随机选择客户端: {target_client.remote_address} (当前可用客户端数: {len(client_list)})")

    # 3. 生成唯一call_id
    call_id = str(uuid.uuid4())

    # 4. 创建异步事件，用于等待客户端响应
    loop = asyncio.get_running_loop()
    future = loop.create_future()
    CALL_CONTEXT[call_id] = future

    try:
        # 5. 转发调用指令给选中的客户端
        call_msg = {
            "cmd": "invoke",
            "group": group,
            "interface": interface,
            "params": params,
            "call_id": call_id,
            "from": "http/local/websocket"
        }
        await target_client.send(json.dumps(call_msg))

        # 6. 等待客户端响应（超时10秒）
        result = await asyncio.wait_for(future, timeout=10.0)
        return result
    except asyncio.TimeoutError:
        # 超时容错：可选逻辑 - 移除超时客户端，或重试其他客户端
        print(f"客户端 {target_client.remote_address} 响应超时，call_id: {call_id}")
        return {"success": False, "error": f"调用超时（10秒），客户端 {target_client.remote_address} 无响应", "call_id": call_id}
    except Exception as e:
        print(f"调用客户端 {target_client.remote_address} 失败: {str(e)}")
        return {"success": False, "error": f"调用失败: {str(e)}", "call_id": call_id}
    finally:
        # 7. 清理调用上下文
        if call_id in CALL_CONTEXT:
            del CALL_CONTEXT[call_id]


# ===================== WebSocket服务端逻辑 =====================
class RPCWebsocketServer:
    async def handle_client(self, websocket: WebSocketServerProtocol, path: str):
        """处理单个WebSocket客户端连接"""
        print(f"新WebSocket客户端连接: {websocket.remote_address}")
        CLIENT_META[websocket] = {"groups": set(), "interfaces": set()}

        try:
            async for message in websocket:
                try:
                    data = json.loads(message)
                    await self.dispatch_message(websocket, data)
                except json.JSONDecodeError:
                    await self.send_response(websocket, error="无效的JSON格式")
                except Exception as e:
                    import traceback
                    traceback.print_exc()
                    await self.send_response(websocket, error=f"处理消息失败: {str(e)}")
        except websockets.exceptions.ConnectionClosed:
            print(f"WebSocket客户端断开连接: {websocket.remote_address}")
        finally:
            self.cleanup_client(websocket)
            if websocket in CLIENT_META:
                del CLIENT_META[websocket]

    async def dispatch_message(self, websocket: WebSocketServerProtocol, data: Dict[str, Any]):
        """消息分发"""
        cmd = data.get("cmd")

        # 1. 处理客户端返回的执行结果
        if cmd is None and "call_id" in data and "success" in data:
            if data["call_id"] in CALL_CONTEXT:
                CALL_CONTEXT[data["call_id"]].set_result(data)
            return

        # 2. 处理注册/心跳/调用指令
        if cmd == "register":
            await self.register_interface(websocket, data)
        elif cmd == "call":
            # WebSocket客户端发起的调用
            call_id = data.get("call_id", "")
            result = await _invoke_interface(
                group=data.get("group"),
                interface=data.get("interface"),
                params=data.get("params", {})
            )
            result["call_id"] = call_id
            await self.send_response(websocket, **result)
        elif cmd == "heartbeat":
            await self.send_response(websocket, success=True, data="pong")
        else:
            await self.send_response(websocket, error=f"未知指令: {cmd}")

    async def register_interface(self, websocket: WebSocketServerProtocol, data: Dict[str, Any]):
        """注册接口"""
        group = data.get("group")
        interface = data.get("interface")
        if not group or not interface:
            return await self.send_response(websocket, error="缺少group或interface参数")

        if group not in INTERFACE_REGISTRY:
            INTERFACE_REGISTRY[group] = {}
        if interface not in INTERFACE_REGISTRY[group]:
            INTERFACE_REGISTRY[group][interface] = set()

        INTERFACE_REGISTRY[group][interface].add(websocket)
        CLIENT_META[websocket]["groups"].add(group)
        CLIENT_META[websocket]["interfaces"].add((group, interface))

        print(
            f"客户端 {websocket.remote_address} 注册接口: {group}.{interface} (当前该接口总客户端数: {len(INTERFACE_REGISTRY[group][interface])})")
        await self.send_response(websocket, success=True,
                                 data=f"注册 {group}.{interface} 成功，当前该接口在线客户端数: {len(INTERFACE_REGISTRY[group][interface])}")

    async def send_response(self, websocket: WebSocketServerProtocol, **kwargs):
        """统一响应格式"""
        response = {"success": kwargs.get("success", False)}
        if "error" in kwargs:
            response["error"] = kwargs["error"]
        if "data" in kwargs:
            response["data"] = kwargs["data"]
        if "call_id" in kwargs:
            response["call_id"] = kwargs["call_id"]
        await websocket.send(json.dumps(response))

    def cleanup_client(self, websocket: WebSocketServerProtocol):
        """清理客户端资源"""
        meta = CLIENT_META.get(websocket, {})
        for group, interface in meta.get("interfaces", []):
            if group in INTERFACE_REGISTRY and interface in INTERFACE_REGISTRY[group]:
                INTERFACE_REGISTRY[group][interface].discard(websocket)
                print(
                    f"客户端 {websocket.remote_address} 下线，接口 {group}.{interface} 剩余客户端数: {len(INTERFACE_REGISTRY[group][interface])}")

        # 修复：正确修改全局变量 CALL_CONTEXT（解决PyCharm报错）
        global CALL_CONTEXT  # 关键：声明使用全局变量
        # 过滤掉已完成的Future，清理无效上下文
        CALL_CONTEXT = {k: v for k, v in CALL_CONTEXT.items() if not v.done()}
        print(f"清理客户端 {websocket.remote_address} 资源完成")


# ===================== FastAPI HTTP接口层 =====================
@app.post("/rpc/call", summary="调用RPC接口（HTTP方式）")
async def rpc_http_call(request: RPCCallRequest = Body(...)):
    """HTTP方式调用客户端RPC接口（支持随机负载均衡）"""
    result = await _invoke_interface(
        group=request.group,
        interface=request.interface,
        params=request.params
    )
    if not result["success"]:
        raise HTTPException(status_code=400, detail=result["error"])
    return result


@app.get("/rpc/list", summary="查询已注册的接口列表")
async def rpc_list_interfaces():
    """查询所有已注册的分组和接口（含客户端数量）"""
    interface_list = {}
    for group, interfaces in INTERFACE_REGISTRY.items():
        interface_list[group] = {
            interface: {
                "client_count": len(clients),
                "client_addresses": [str(client.remote_address) for client in clients]
            } for interface, clients in interfaces.items()
        }
    return {
        "success": True,
        "data": interface_list,
        "message": "已注册接口列表（含客户端信息）"
    }


# ===================== 本地Python调用函数 =====================
async def rpc_call(group: str, interface: str, params: Dict[str, Any] = {}) -> Dict[str, Any]:
    """本地Python模块调用RPC接口（支持随机负载均衡）"""
    return await _invoke_interface(group, interface, params)


def rpc_call_sync(group: str, interface: str, params: Dict[str, Any] = {}) -> Dict[str, Any]:
    """同步版本的本地调用函数"""
    if sys.platform == 'win32':
        asyncio.set_event_loop_policy(asyncio.WindowsSelectorEventLoopPolicy())
    loop = asyncio.new_event_loop()
    asyncio.set_event_loop(loop)
    try:
        result = loop.run_until_complete(_invoke_interface(group, interface, params))
        return result
    finally:
        loop.close()


# ===================== 服务启动逻辑 =====================
async def start_services():
    """启动WebSocket服务 + FastAPI服务"""
    ws_host = "0.0.0.0"
    ws_port = 8765
    http_host = "0.0.0.0"
    http_port = 8000

    ws_server = RPCWebsocketServer()
    ws_start = websockets.serve(ws_server.handle_client, ws_host, ws_port)

    config = uvicorn.Config(app, host=http_host, port=http_port)
    http_server = uvicorn.Server(config)

    await asyncio.gather(
        ws_start,
        http_server.serve()
    )


if __name__ == "__main__":

    # 初始化随机种子（保证随机性）
    random.seed()

    asyncio.run(start_services())