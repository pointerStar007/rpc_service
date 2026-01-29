#!/usr/bin/env python
# -*- coding: utf-8 -*-
# @Time    : 2026/1/27 21:16
# @Author  : Pointer
# @File    : websocket_call_demo.py
# @Software: PyCharm
import json
import asyncio
import websockets
import uuid


async def call_md5_interface():
    """调用MD5加密RPC接口"""
    ws_url = "ws://127.0.0.1:8765"
    # 加密接口的分组和接口名（和客户端注册的一致）
    target_group = "encrypt_group"
    target_interface = "md5_encrypt"

    # 调用参数：待加密的字符串（content为固定参数名）
    call_params = {
        "content": "1"  # 可替换为任意待加密字符串
    }

    try:
        async with websockets.connect(ws_url) as websocket:
            print(f"需求端已连接到RPC服务端: {ws_url}")

            # 生成唯一调用ID
            call_id = str(uuid.uuid4())

            # 构造MD5加密调用指令
            call_msg = {
                "cmd": "call",
                "group": target_group,
                "interface": target_interface,
                "params": call_params,
                "call_id": call_id
            }

            # 发送调用指令
            print(f"\n=== 发送MD5加密调用指令 ===")
            print(json.dumps(call_msg, ensure_ascii=False, indent=2))
            await websocket.send(json.dumps(call_msg))

            # 接收服务端转发确认
            response1 = await websocket.recv()
            response1_data = json.loads(response1)
            print(f"\n=== 收到服务端转发确认 ===")
            print(json.dumps(response1_data, ensure_ascii=False, indent=2))


    except Exception as e:
        print(f"调用失败: {str(e)}")


if __name__ == "__main__":
    asyncio.run(call_md5_interface())