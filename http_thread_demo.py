#!/usr/bin/env python
# -*- coding: utf-8 -*-
# @Time    : 2026/1/27 22:56
# @Author  : Pointer
# @File    : http_thread_demo.py
# @Software: PyCharm


import time
import requests
import statistics
from concurrent.futures import ThreadPoolExecutor, as_completed
from requests.adapters import HTTPAdapter

# ===================== 性能测试配置（核心：最小化客户端干扰） =====================
# 1. 并发数（模拟真实用户数，建议从5/10/20逐步增加）
CONCURRENT_USERS = 20
# 2. 每个用户的请求数（总请求数=CONCURRENT_USERS*REQUESTS_PER_USER）
REQUESTS_PER_USER = 1000
# 3. 总请求数（自动计算）
TOTAL_REQUESTS = CONCURRENT_USERS * REQUESTS_PER_USER
# 4. 请求超时时间（避免慢请求阻塞）
TIMEOUT = 5
# 5. 目标接口地址
RPC_URL = "http://127.0.0.1:8000/rpc/call"


# ===================== 高性能Session配置（消除客户端瓶颈） =====================
def create_perf_session():
    """创建高性能、低干扰的Session（仅保留必要配置，避免客户端成为瓶颈）"""
    session = requests.Session()
    # 1. 极简连接池（仅复用连接，不重试！重试会干扰性能统计）
    adapter = HTTPAdapter(
        pool_connections=CONCURRENT_USERS,
        pool_maxsize=CONCURRENT_USERS,
        max_retries=0,  # 性能测试不重试：重试会导致响应时间统计失真
    )
    session.mount("http://", adapter)
    session.headers.update({
        "Content-Type": "application/json",
        "Connection": "keep-alive"  # 长连接：消除TCP握手耗时
    })
    return session


# 全局Session（所有线程共享，消除连接创建开销）
SESSION = create_perf_session()


# ===================== 带精准计时的请求函数 =====================
def perf_test_task(user_id, request_id):
    """单请求任务：返回精准的响应时间和结果（无多余日志）"""
    task_id = f"{user_id}-{request_id}"
    data = {
        "group": "encrypt_group",
        "interface": "md5_encrypt",
        "params": {"content": task_id}
    }

    # 精准计时：仅统计接口响应时间（排除数据构造等耗时）
    start_time = time.perf_counter()  # 高精度计时（比time.time更准）
    try:
        response = SESSION.post(RPC_URL, json=data, timeout=TIMEOUT)
        response.raise_for_status()
        response.json()  # 确保解析完成（统计完整响应时间）
        elapsed = (time.perf_counter() - start_time) * 1000  # 转毫秒
        return {
            "success": True,
            "task_id": task_id,
            "response_time_ms": round(elapsed, 2),
            "error": None
        }
    except Exception as e:
        elapsed = (time.perf_counter() - start_time) * 1000
        return {
            "success": False,
            "task_id": task_id,
            "response_time_ms": round(elapsed, 2),
            "error": str(e)[:50]  # 截断错误信息
        }


# ===================== 性能测试主逻辑 =====================
if __name__ == '__main__':
    print(f"===== 接口性能测试开始 =====")
    print(f"并发用户数：{CONCURRENT_USERS}")
    print(f"每个用户请求数：{REQUESTS_PER_USER}")
    print(f"总请求数：{TOTAL_REQUESTS}")
    print(f"目标接口：{RPC_URL}")
    print("-" * 50)

    # 存储所有请求的性能数据
    all_results = []
    start_total = time.perf_counter()

    # 启动线程池（模拟并发用户）
    with ThreadPoolExecutor(max_workers=CONCURRENT_USERS) as executor:
        # 提交所有任务：每个用户发REQUESTS_PER_USER次请求
        futures = []
        for user_id in range(CONCURRENT_USERS):
            for request_id in range(REQUESTS_PER_USER):
                futures.append(executor.submit(perf_test_task, user_id, request_id))

        # 收集结果（实时统计）
        completed = 0
        for future in as_completed(futures):
            result = future.result()
            all_results.append(result)
            completed += 1

            # 每完成10%打印一次进度（减少日志干扰）
            if completed % (TOTAL_REQUESTS // 10) == 0 or completed == TOTAL_REQUESTS:
                progress = (completed / TOTAL_REQUESTS) * 100
                print(f"进度：{completed}/{TOTAL_REQUESTS} ({progress:.1f}%)")

    # 计算总耗时
    total_elapsed = (time.perf_counter() - start_total) * 1000  # 转毫秒
    qps = TOTAL_REQUESTS / (total_elapsed / 1000)  # 每秒请求数

    # 提取有效数据（仅成功请求）
    success_results = [r for r in all_results if r["success"]]
    fail_results = [r for r in all_results if not r["success"]]
    response_times = [r["response_time_ms"] for r in success_results]

    # ===================== 输出精准的性能报告 =====================
    print("\n===== 性能测试报告 =====")
    print(f"1. 整体指标：")
    print(f"   - 总请求数：{TOTAL_REQUESTS}")
    print(f"   - 成功数：{len(success_results)} ({len(success_results) / TOTAL_REQUESTS * 100:.2f}%)")
    print(f"   - 失败数：{len(fail_results)}")
    print(f"   - 总耗时：{total_elapsed:.2f} 毫秒")
    print(f"   - QPS（每秒请求数）：{qps:.2f}")

    if response_times:
        print(f"\n2. 响应时间（仅成功请求，单位：毫秒）：")
        print(f"   - 平均值：{statistics.mean(response_times):.2f}")
        print(f"   - 中位数：{statistics.median(response_times):.2f}")
        print(f"   - 最小值：{min(response_times):.2f}")
        print(f"   - 最大值：{max(response_times):.2f}")
        # 计算95/99分位（反映长尾延迟）
        sorted_times = sorted(response_times)
        p95 = sorted_times[int(len(sorted_times) * 0.95)]
        p99 = sorted_times[int(len(sorted_times) * 0.99)]
        print(f"   - 95分位：{p95:.2f}")
        print(f"   - 99分位：{p99:.2f}")

    if fail_results:
        print(f"\n3. 失败详情（前5条）：")
        for i, fail in enumerate(fail_results[:5]):
            print(f"   - {fail['task_id']}：{fail['error']}")