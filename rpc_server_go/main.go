package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// 全局核心数据结构（精简锁竞争，保留并发安全，无冗余缓存）
var (
	interfaceRegistry = make(map[string]map[string]*sync.Map)
	registryMu        sync.RWMutex
	clientMeta        sync.Map // 替代map+metaMu，减少全局锁竞争
	callContext       = make(map[string]chan *InvokeResult)
	contextMu         sync.RWMutex

	// WebSocket升级器：与Python版1:1兼容，全跨域、无缓冲
	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true // 允许所有跨域，适配Python客户端
		},
		ReadBufferSize:  0,
		WriteBufferSize: 0,
		Subprotocols:    []string{},
	}

	// 服务端口配置：默认与Python版一致（8765/8000），支持命令行自定义
	wsPort   int
	httpPort int
)

// ClientMeta 客户端元数据：仅保留核心字段，连接专属写锁杜绝并发写panic
type ClientMeta struct {
	Groups     map[string]struct{}
	Interfaces map[string]map[string]struct{}
	WriteMu    sync.Mutex // 核心保留：每个连接专属写锁，无并发写风险
}

// InvokeResult 统一响应结构体：与Python版消息协议完全对齐，无修改
type InvokeResult struct {
	Success bool                   `json:"success"`
	Error   string                 `json:"error,omitempty"`
	Data    interface{}            `json:"data,omitempty"`
	CallID  string                 `json:"call_id,omitempty"`
	Params  map[string]interface{} `json:"params,omitempty"`
	Message string                 `json:"message,omitempty"`
}

// WsMessage WebSocket消息结构体：补充From字段，与Python版协议1:1兼容
type WsMessage struct {
	Cmd       string                 `json:"cmd,omitempty"`
	Group     string                 `json:"group,omitempty"`
	Interface string                 `json:"interface,omitempty"`
	Params    map[string]interface{} `json:"params,omitempty"`
	CallID    string                 `json:"call_id,omitempty"`
	Success   bool                   `json:"success,omitempty"`
	Error     string                 `json:"error,omitempty"`
	Data      interface{}            `json:"data,omitempty"`
	From      string                 `json:"from,omitempty"`
}

// 初始化：注册命令行参数，初始化全局结构
func init() {
	flag.IntVar(&wsPort, "ws-port", 8765, "WebSocket service port (与Python版默认一致)")
	flag.IntVar(&httpPort, "http-port", 8000, "HTTP service port (与Python版默认一致)")
	flag.Parse()

	registryMu.Lock()
	defer registryMu.Unlock()
	interfaceRegistry = make(map[string]map[string]*sync.Map)
}

// safeWsWriteJSON 极简稳定版：并发安全的WebSocket写方法
// 仅保留核心写锁逻辑，无冗余校验，性能与安全兼顾
func safeWsWriteJSON(conn *websocket.Conn, data interface{}) error {
	// 从sync.Map快速获取客户端元数据
	metaVal, exist := clientMeta.Load(conn)
	if !exist {
		return fmt.Errorf("客户端连接已断开（元数据已清理）")
	}

	// 加连接专属写锁，自动解锁，确保同一连接串行写
	meta := metaVal.(*ClientMeta)
	meta.WriteMu.Lock()
	defer meta.WriteMu.Unlock()

	// 直接执行写操作，无任何中间冗余步骤
	return conn.WriteJSON(data)
}

// invokeInterface 核心调用函数：精简缓存逻辑，直接遍历sync.Map
// 无预热开销，无缓存维护成本，执行效率稳定，负载均衡逻辑与Python版一致
func invokeInterface(group, iface string, params map[string]interface{}) *InvokeResult {
	// 1. 校验接口分组是否存在
	registryMu.RLock()
	groupMap, gExist := interfaceRegistry[group]
	registryMu.RUnlock()
	if !gExist {
		return &InvokeResult{
			Success: false,
			Error:   fmt.Sprintf("接口 %s.%s 未注册", group, iface),
			CallID:  "",
		}
	}

	// 2. 校验接口是否存在
	registryMu.RLock()
	clientMap, iExist := groupMap[iface]
	registryMu.RUnlock()
	if !iExist || clientMap == nil {
		return &InvokeResult{
			Success: false,
			Error:   fmt.Sprintf("接口 %s.%s 未注册", group, iface),
			CallID:  "",
		}
	}

	// 3. 遍历sync.Map收集可用客户端（无缓存，直接遍历，稳定无开销）
	var clients []*websocket.Conn
	clientMap.Range(func(key, _ interface{}) bool {
		conn, ok := key.(*websocket.Conn)
		if ok {
			clients = append(clients, conn)
		}
		return true
	})

	// 4. 校验客户端是否可用
	if len(clients) == 0 {
		return &InvokeResult{
			Success: false,
			Error:   fmt.Sprintf("接口 %s.%s 暂无可用客户端", group, iface),
			CallID:  "",
		}
	}

	// 5. 随机选择客户端（负载均衡，与Python版random.choice逻辑一致）
	targetClient := clients[time.Now().UnixNano()%int64(len(clients))]
	log.Printf("负载均衡 - 随机选择客户端: %s (当前可用客户端数: %d)", targetClient.RemoteAddr(), len(clients))

	// 6. 生成唯一CallID，创建结果通道（替代Python版asyncio.Future）
	callID := uuid.NewString()
	resultChan := make(chan *InvokeResult, 1)
	contextMu.Lock()
	callContext[callID] = resultChan
	contextMu.Unlock()

	// 7. 延迟清理调用上下文，避免内存泄漏
	defer func() {
		contextMu.Lock()
		delete(callContext, callID)
		contextMu.Unlock()
		close(resultChan)
	}()

	// 8. 构造调用指令，与Python版协议格式完全一致
	callMsg := WsMessage{
		Cmd:       "invoke",
		Group:     group,
		Interface: iface,
		Params:    params,
		CallID:    callID,
		From:      "http/local/websocket",
	}

	// 9. 并发安全发送指令（核心写锁保护，无panic）
	if err := safeWsWriteJSON(targetClient, callMsg); err != nil {
		log.Printf("发送指令到客户端 %s 失败: %v", targetClient.RemoteAddr(), err)
		return &InvokeResult{
			Success: false,
			Error:   fmt.Sprintf("调用失败: 发送指令失败 - %v", err),
			CallID:  callID,
		}
	}

	// 10. 10秒超时控制（与Python版asyncio.wait_for逻辑一致）
	select {
	case result := <-resultChan:
		return result
	case <-time.After(10 * time.Second):
		log.Printf("客户端 %s 响应超时，call_id: %s", targetClient.RemoteAddr(), callID)
		return &InvokeResult{
			Success: false,
			Error:   fmt.Sprintf("调用超时（10秒），客户端 %s 无响应", targetClient.RemoteAddr()),
			CallID:  callID,
		}
	}
}

// handleWsClient 处理WebSocket客户端连接：精简逻辑，稳定高效
func handleWsClient(w http.ResponseWriter, r *http.Request) {
	// 升级HTTP连接为WebSocket（根路径，Python客户端直接连接）
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket升级失败: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	defer conn.Close()

	clientAddr := conn.RemoteAddr().String()
	log.Printf("新WebSocket客户端连接: %s", clientAddr)

	// 初始化客户端元数据，存入sync.Map（无全局锁竞争）
	clientMeta.Store(conn, &ClientMeta{
		Groups:     make(map[string]struct{}),
		Interfaces: make(map[string]map[string]struct{}),
		WriteMu:    sync.Mutex{}, // 显式初始化写锁，规范无歧义
	})
	// 连接关闭时清理元数据
	defer clientMeta.Delete(conn)

	// 循环读取客户端消息，稳定处理
	for {
		var msg WsMessage
		if err := conn.ReadJSON(&msg); err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				log.Printf("WebSocket客户端正常断开: %s", clientAddr)
			} else {
				log.Printf("WebSocket读取消息失败: %s, 错误: %v", clientAddr, err)
			}
			// 客户端断开，清理资源
			cleanupClient(conn)
			return
		}

		// 分发处理消息，错误友好响应
		if err := dispatchWsMessage(conn, &msg); err != nil {
			log.Printf("处理客户端 %s 消息失败: %v", clientAddr, err)
			_ = safeWsWriteJSON(conn, &InvokeResult{
				Success: false,
				Error:   fmt.Sprintf("处理消息失败: %v", err),
			})
		}
	}
}

// dispatchWsMessage 消息分发：精简逻辑，保留所有协议指令，与Python版100%兼容
func dispatchWsMessage(conn *websocket.Conn, msg *WsMessage) error {
	// 1. 处理客户端返回的执行结果（无cmd，含call_id和执行状态）
	if msg.Cmd == "" && msg.CallID != "" && (msg.Success || msg.Error != "") {
		contextMu.RLock()
		resultChan, exist := callContext[msg.CallID]
		contextMu.RUnlock()
		if exist {
			resultChan <- &InvokeResult{
				Success: msg.Success,
				Error:   msg.Error,
				Data:    msg.Data,
				CallID:  msg.CallID,
			}
		}
		return nil
	}

	// 2. 处理接口注册指令（cmd=register）
	if msg.Cmd == "register" {
		if msg.Group == "" || msg.Interface == "" {
			return safeWsWriteJSON(conn, &InvokeResult{
				Success: false,
				Error:   "缺少group或interface参数",
			})
		}
		return registerInterface(conn, msg.Group, msg.Interface)
	}

	// 3. 处理WebSocket客户端发起的接口调用（cmd=call）
	if msg.Cmd == "call" {
		result := invokeInterface(msg.Group, msg.Interface, msg.Params)
		result.CallID = msg.CallID
		return safeWsWriteJSON(conn, result)
	}

	// 4. 处理心跳检测（cmd=heartbeat），返回pong，与Python版一致
	if msg.Cmd == "heartbeat" {
		return safeWsWriteJSON(conn, &InvokeResult{
			Success: true,
			Data:    "pong",
		})
	}

	// 5. 处理未知指令
	return safeWsWriteJSON(conn, &InvokeResult{
		Success: false,
		Error:   fmt.Sprintf("未知指令: %s", msg.Cmd),
	})
}

// registerInterface 接口注册：精简逻辑，无缓存更新，稳定高效
func registerInterface(conn *websocket.Conn, group, iface string) error {
	registryMu.Lock()
	defer registryMu.Unlock()

	// 初始化分组（若不存在）
	if _, exist := interfaceRegistry[group]; !exist {
		interfaceRegistry[group] = make(map[string]*sync.Map)
	}

	// 初始化接口客户端Map（若不存在）
	if _, exist := interfaceRegistry[group][iface]; !exist {
		interfaceRegistry[group][iface] = &sync.Map{}
	}

	// 将客户端加入接口注册表
	clientMap := interfaceRegistry[group][iface]
	clientMap.Store(conn, struct{}{})

	// 更新客户端元数据
	metaVal, exist := clientMeta.Load(conn)
	if !exist {
		return fmt.Errorf("客户端元数据不存在")
	}
	meta := metaVal.(*ClientMeta)
	meta.Groups[group] = struct{}{}
	if _, exist := meta.Interfaces[group]; !exist {
		meta.Interfaces[group] = make(map[string]struct{})
	}
	meta.Interfaces[group][iface] = struct{}{}

	// 统计当前接口客户端数
	clientCount := 0
	clientMap.Range(func(_, _ interface{}) bool {
		clientCount++
		return true
	})

	log.Printf("客户端 %s 注册接口: %s.%s (当前该接口总客户端数: %d)", conn.RemoteAddr(), group, iface, clientCount)

	// 返回注册成功响应（并发安全写）
	return safeWsWriteJSON(conn, &InvokeResult{
		Success: true,
		Data:    fmt.Sprintf("注册 %s.%s 成功，当前该接口在线客户端数: %d", group, iface, clientCount),
	})
}

// cleanupClient 客户端资源清理：精简逻辑，无缓存更新，彻底清理无残留
func cleanupClient(conn *websocket.Conn) {
	clientAddr := conn.RemoteAddr().String()

	// 获取客户端元数据，用于清理注册表
	metaVal, exist := clientMeta.Load(conn)
	if !exist {
		log.Printf("客户端 %s 元数据已清理，无需重复处理", clientAddr)
		return
	}
	meta := metaVal.(*ClientMeta)

	// 从接口注册表移除客户端，清理资源
	registryMu.Lock()
	for group, ifaces := range meta.Interfaces {
		for iface := range ifaces {
			if groupMap, gExist := interfaceRegistry[group]; gExist {
				if clientMap, iExist := groupMap[iface]; iExist {
					clientMap.Delete(conn)
					// 统计剩余客户端数
					remainCount := 0
					clientMap.Range(func(_, _ interface{}) bool {
						remainCount++
						return true
					})
					log.Printf("客户端 %s 下线，接口 %s.%s 剩余客户端数: %d", clientAddr, group, iface, remainCount)
				}
			}
		}
	}
	registryMu.Unlock()

	// 清理调用上下文中的未完成请求，避免内存泄漏
	contextMu.Lock()
	for callID, ch := range callContext {
		select {
		case ch <- &InvokeResult{
			Success: false,
			Error:   fmt.Sprintf("客户端 %s 已断开", clientAddr),
			CallID:  callID,
		}:
		default:
		}
		delete(callContext, callID)
		close(ch)
	}
	contextMu.Unlock()

	log.Printf("客户端 %s 资源清理完成", clientAddr)
}

// httpRpcCall HTTP调用接口：无任何限流，完全释放Go高并发能力
// 与Python版/rpc/call接口逻辑、返回格式完全一致
func httpRpcCall(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	// 解析POST请求参数
	var req struct {
		Group     string                 `json:"group"`
		Interface string                 `json:"interface"`
		Params    map[string]interface{} `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		_ = json.NewEncoder(w).Encode(&InvokeResult{
			Success: false,
			Error:   fmt.Sprintf("解析请求失败: %v", err),
		})
		return
	}
	defer r.Body.Close()

	// 调用核心逻辑，返回结果
	result := invokeInterface(req.Group, req.Interface, req.Params)
	if !result.Success {
		w.WriteHeader(http.StatusBadRequest) // 与Python版400错误码对齐
	}
	_ = json.NewEncoder(w).Encode(result)
}

// httpRpcList 接口查询：与Python版返回格式、字段完全一致
func httpRpcList(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	interfaceList := make(map[string]map[string]interface{})
	registryMu.RLock()
	for group, ifaces := range interfaceRegistry {
		groupData := make(map[string]interface{})
		for iface, clientMap := range ifaces {
			clientCount := 0
			clientAddrs := make([]string, 0)
			clientMap.Range(func(key, _ interface{}) bool {
				conn, ok := key.(*websocket.Conn)
				if ok {
					clientCount++
					clientAddrs = append(clientAddrs, conn.RemoteAddr().String())
				}
				return true
			})
			groupData[iface] = map[string]interface{}{
				"client_count":     clientCount,
				"client_addresses": clientAddrs,
			}
		}
		interfaceList[group] = groupData
	}
	registryMu.RUnlock()

	_ = json.NewEncoder(w).Encode(&InvokeResult{
		Success: true,
		Data:    interfaceList,
		Message: "已注册接口列表（含客户端信息）",
	})
}

// startServices 启动服务：精简优雅关闭逻辑，稳定可靠
func startServices() {
	// 注册路由：与Python版完全一致，无任何修改
	http.HandleFunc("/", handleWsClient)
	http.HandleFunc("POST /rpc/call", httpRpcCall)
	http.HandleFunc("GET /rpc/list", httpRpcList)

	// 启动WebSocket服务（独立goroutine，不阻塞主流程）
	go func() {
		wsAddr := fmt.Sprintf("0.0.0.0:%d", wsPort)
		log.Printf("WebSocket服务启动成功，监听地址: %s", wsAddr)
		if err := http.ListenAndServe(wsAddr, nil); err != nil && err != http.ErrServerClosed {
			log.Fatalf("WebSocket服务启动失败: %v", err)
		}
	}()

	// 启动HTTP服务（独立goroutine，不阻塞主流程）
	go func() {
		httpAddr := fmt.Sprintf("0.0.0.0:%d", httpPort)
		log.Printf("HTTP服务启动成功，监听地址: %s", httpAddr)
		if err := http.ListenAndServe(httpAddr, nil); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP服务启动失败: %v", err)
		}
	}()

	// 优雅退出：监听系统信号（Ctrl+C/杀死进程），安全清理资源
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("开始优雅关闭服务...")

	// 关闭所有客户端连接，发送关闭帧
	clientMeta.Range(func(key, _ interface{}) bool {
		conn := key.(*websocket.Conn)
		closeMsg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "服务端优雅关闭")
		_ = conn.WriteControl(websocket.CloseMessage, closeMsg, time.Now().Add(1*time.Second))
		_ = conn.Close()
		return true
	})

	log.Println("服务已优雅关闭，所有资源清理完成")
}

// main 主函数：极简，仅启动服务，无任何冗余逻辑
func main() {
	startServices()
}
