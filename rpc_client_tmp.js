
/**
 * 纯JS RPC客户端模板（无UI、可移植）
 * 适配WebSocket RPC服务端，支持接口注册、心跳保活、服务调用响应
 * 环境：浏览器（原生WebSocket）/ Node.js（需安装ws：npm install ws）
 */
class RPCClient {
    /**
     * 构造函数
     * @param {string} wsUrl - WebSocket服务端地址（如：ws://127.0.0.1:8765）
     * @param {Object} options - 配置项
     * @param {number} options.heartbeatInterval - 心跳间隔（秒，默认30）
     * @param {number} options.reconnectInterval - 重连间隔（秒，默认5）
     * @param {boolean} options.autoReconnect - 是否自动重连（默认true）
     */
    constructor(wsUrl, options = {}) {
        // 基础配置
        this.wsUrl = wsUrl;
        this.heartbeatInterval = (options.heartbeatInterval || 30) * 1000;
        this.reconnectInterval = (options.reconnectInterval || 5) * 1000;
        this.autoReconnect = options.autoReconnect !== false;

        // 核心状态
        this.ws = null; // WebSocket实例
        this.isConnected = false; // 连接状态
        this.heartbeatTimer = null; // 心跳定时器
        this.registeredInterfaces = new Map(); // 已注册的接口：key=group.interface，value=处理函数

        // 环境兼容（Node.js/浏览器）
        this.WebSocket = typeof window !== 'undefined' ? window.WebSocket : require('ws');
    }

    /**
     * 初始化连接
     * @returns {Promise} 连接成功/失败Promise
     */
    connect() {
        return new Promise((resolve, reject) => {
            // 关闭已有连接
            if (this.ws) {
                this.ws.close();
            }

            // 创建新连接
            try {
                this.ws = new this.WebSocket(this.wsUrl);
            } catch (e) {
                reject(new Error(`创建WebSocket连接失败：${e.message}`));
                return;
            }

            // 连接成功回调
            this.ws.onopen = () => {
                this.isConnected = true;
                console.log(`RPC客户端已连接到：${this.wsUrl}`);

                // 重启心跳
                this.startHeartbeat();

                // 重新注册已声明的接口（重连后恢复）
                this.reRegisterInterfaces();

                resolve();
            };

            // 连接关闭回调
            this.ws.onclose = (event) => {
                this.isConnected = false;
                console.log(`RPC连接关闭：代码=${event.code}，原因=${event.reason}`);

                // 清除定时器
                this.stopHeartbeat();

                // 自动重连
                if (this.autoReconnect) {
                    console.log(`${this.reconnectInterval / 1000}秒后尝试重连...`);
                    setTimeout(() => this.connect().catch(err => console.error('重连失败：', err)), this.reconnectInterval);
                }
            };

            // 错误回调
            this.ws.onerror = (error) => {
                this.isConnected = false;
                reject(new Error(`WebSocket连接错误：${error.message}`));
            };

            // 消息处理（核心：响应服务端的接口调用）
            this.ws.onmessage = (event) => {
                this.handleServerMessage(event.data);
            };
        });
    }

    /**
     * 处理服务端消息
     * @param {string} data - 服务端发送的JSON字符串
     */
    handleServerMessage(data) {
        try {
            const msg = JSON.parse(data);

            //  处理接口调用指令
            if (msg.cmd === 'invoke') {
                this.handleInterfaceInvoke(msg);
            }
            if (msg.success !== undefined && msg.data !== "pong") {
                console.log(msg)
            }
        } catch (e) {
            console.error('解析服务端消息失败：', e, '原始数据：', data);
        }
    }

    /**
     * 处理服务端的接口调用
     * @param {Object} invokeMsg - 调用指令 {cmd: 'invoke', group: '', interface: '', params: {}, call_id: ''}
     */
    handleInterfaceInvoke(invokeMsg) {
        const {group, interface: interfaceName, params, call_id} = invokeMsg;
        const key = `${group}.${interfaceName}`;

        // 查找已注册的接口处理函数
        const handler = this.registeredInterfaces.get(key);
        if (!handler) {
            this.sendResponse(call_id, false, `未注册的接口：${key}`);
            return;
        }

        // 执行接口处理函数并返回结果
        try {
            // 支持同步/异步处理函数
            const result = handler(params);
            if (result instanceof Promise) {
                // 异步函数
                result.then(data => this.sendResponse(call_id, true, null, data))
                    .catch(err => this.sendResponse(call_id, false, err.message));
            } else {
                // 同步函数
                this.sendResponse(call_id, true, null, result);
            }
        } catch (err) {
            this.sendResponse(call_id, false, err.message);
        }
    }

    /**
     * 发送响应给服务端
     * @param {string} callId - 调用ID
     * @param {boolean} success - 是否成功
     * @param {string} error - 错误信息（失败时）
     * @param {any} data - 响应数据（成功时）
     */
    sendResponse(callId, success, error = null, data = null) {
        if (!this.isConnected) {
            console.error('无法发送响应：RPC连接已关闭');
            return;
        }

        const response = {
            call_id: callId,
            success,
            error,
            data
        };

        this.ws.send(JSON.stringify(response));
    }

    /**
     * 注册接口处理函数
     * @param {string} group - 分组名
     * @param {string} interfaceName - 接口名
     * @param {Function} handler - 处理函数（支持同步/异步），入参：params，返回：结果数据
     */
    registerInterface(group, interfaceName, handler) {
        const key = `${group}.${interfaceName}`;
        this.registeredInterfaces.set(key, handler);

        // 发送注册指令给服务端
        if (this.isConnected) {
            this.ws.send(JSON.stringify({
                cmd: 'register',
                group,
                interface: interfaceName
            }));
        }
    }

    /**
     * 重连后重新注册所有接口
     */
    reRegisterInterfaces() {
        this.registeredInterfaces.forEach((handler, key) => {
            const [group, interfaceName] = key.split('.');
            this.registerInterface(group, interfaceName, handler);
        });
    }

    /**
     * 启动心跳保活
     */
    startHeartbeat() {
        this.stopHeartbeat(); // 先清除旧定时器
        this.heartbeatTimer = setInterval(() => {
            if (this.isConnected) {
                this.ws.send(JSON.stringify({cmd: 'heartbeat'}));
            }
        }, this.heartbeatInterval);
    }

    /**
     * 停止心跳保活
     */
    stopHeartbeat() {
        if (this.heartbeatTimer) {
            clearInterval(this.heartbeatTimer);
            this.heartbeatTimer = null;
        }
    }

    /**
     * 主动关闭连接
     */
    disconnect() {
        this.autoReconnect = false; // 关闭自动重连
        this.stopHeartbeat();
        if (this.ws) {
            this.ws.close(1000, '客户端主动关闭');
        }
        this.registeredInterfaces.clear();
        console.log('RPC客户端已主动断开连接');
    }
}


// ==========================以下为演示===============================

function add(num1,num2) {
    // 同步模块示例
    console.log("同步模块调用 add",num1,num2)
    return {result: num1 + num2}
}
//
async function async_add(num1,num2) {
    // 异步模块示例
    console.log("异步模块调用 async_add",num1,num2)
    return {result: num1 + num2}
}
function async_add2(num1,num2){
    return new Promise(function (resolve, reject) {
        console.log("异步模块调用 async_add2",num1,num2)
        resolve({result: num1 + num2})
    })
}
// // 使用示例

async function main() {
    // 1. 创建客户端
    const client = new RPCClient('ws://127.0.0.1:8765');

// 2. 注册业务接口（核心：替换为你的业务逻辑）
    client.registerInterface('math', 'add', (params)=>{
        console.log(params)
        return add(params.num1,params.num2)
    });
    client.registerInterface('math', 'async_add', async (params)=>{
        console.log(params)
        return await async_add(params.num1,params.num2)
    });
    client.registerInterface('math', 'async_add2', async (params)=>{
        console.log(params)
        return await async_add2(params.num1,params.num2)
    });
// 3. 启动连接
    await client.connect();
}

main()