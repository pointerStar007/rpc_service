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

// 全局核心数据结构（并发安全，与Python版逻辑一致）
var (
	interfaceRegistry = make(map[string]map[string]*sync.Map)
	registryMu        sync.RWMutex
	clientMeta        = make(map[*websocket.Conn]*ClientMeta)
	metaMu            sync.RWMutex
	callContext       = make(map[string]chan *InvokeResult)
	contextMu         sync.RWMutex

	// WebSocket升级器：完全对齐Python版行为（根路径、无缓冲、全跨域）
	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true // 允许所有跨域，与Python版一致
		},
		ReadBufferSize:  0, // 关闭读缓冲限制，与Python版一致
		WriteBufferSize: 0, // 关闭写缓冲限制，与Python版一致
		Subprotocols:    []string{}, // 无子协议，与Python版一致
	}

	// 服务端口配置：与Python版默认值一致（8765/8000），支持命令行自定义
	wsPort   int
	httpPort int
)

// ClientMeta 客户端元数据，记录注册的分组和接口（与Python版 CLIENT_META 对齐）
type ClientMeta struct {
	Groups     map[string]struct{}
	Interfaces map[string]map[string]struct{}
}

// InvokeResult 统一响应结构体：补充Message字段，与Python版响应格式1:1对齐
type InvokeResult struct {
	Success bool                   `json:"success"`
	Error   string                 `json:"error,omitempty"`
	Data    interface{}            `json:"data,omitempty"`
	CallID  string                 `json:"call_id,omitempty"`
	Params  map[string]interface{} `json:"params,omitempty"`
	Message string                 `json:"message,omitempty"` // 适配Python版关键字段
}

// WsMessage WebSocket消息结构体：补充From字段，与Python版消息协议1:1对齐
type WsMessage struct {
	Cmd       string                 `json:"cmd,omitempty"`
	Group     string                 `json:"group,omitempty"`
	Interface string                 `json:"interface,omitempty"`
	Params    map[string]interface{} `json:"params,omitempty"`
	CallID    string                 `json:"call_id,omitempty"`
	Success   bool                   `json:"success,omitempty"`
	Error     string                 `json:"error,omitempty"`
	Data      interface{}            `json:"data,omitempty"`
	From      string                 `json:"from,omitempty"` // 适配Python版关键字段
}

// 初始化：注册命令行参数，初始化全局结构（与Python版启动逻辑一致）
func init() {
	flag.IntVar(&wsPort, "ws-port", 8765, "WebSocket service port (与Python版默认一致)")
	flag.IntVar(&httpPort, "http-port", 8000, "HTTP service port (与Python版默认一致)")
	flag.Parse()

	registryMu.Lock()
	defer registryMu.Unlock()
	interfaceRegistry = make(map[string]map[string]*sync.Map)
}

// invokeInterface 核心调用函数：完全复刻Python版 _invoke_interface，随机负载均衡+10秒超时
func invokeInterface(group, iface string, params map[string]interface{}) *InvokeResult {
	// 校验接口是否注册
	registryMu.RLock()
	groupMap, groupExist := interfaceRegistry[group]
	if !groupExist {
		registryMu.RUnlock()
		return &InvokeResult{
			Success: false,
			Error:   fmt.Sprintf("接口 %s.%s 未注册", group, iface),
			CallID:  "",
		}
	}
	clientMap, ifaceExist := groupMap[iface]
	registryMu.RUnlock()

	if !ifaceExist || clientMap == nil {
		return &InvokeResult{
			Success: false,
			Error:   fmt.Sprintf("接口 %s.%s 未注册", group, iface),
			CallID:  "",
		}
	}

	// 提取可用客户端，随机选择（负载均衡核心，与Python版一致）
	var clients []*websocket.Conn
	clientMap.Range(func(key, value interface{}) bool {
		conn, ok := key.(*websocket.Conn)
		if ok {
			clients = append(clients, conn)
		}
		return true
	})

	if len(clients) == 0 {
		return &InvokeResult{
			Success: false,
			Error:   fmt.Sprintf("接口 %s.%s 暂无可用客户端", group, iface),
			CallID:  "",
		}
	}

	// 随机选择客户端（与Python版 random.choice 逻辑一致）
	targetClient := clients[time.Now().UnixNano()%int64(len(clients))]
	log.Printf("负载均衡 - 随机选择客户端: %s (当前可用客户端数: %d)", targetClient.RemoteAddr(), len(clients))

	// 生成唯一CallID，创建结果通道（替代Python版 asyncio.Future）
	callID := uuid.NewString()
	resultChan := make(chan *InvokeResult, 1)
	contextMu.Lock()
	callContext[callID] = resultChan
	contextMu.Unlock()

	// 延迟清理调用上下文，与Python版 finally 逻辑一致
	defer func() {
		contextMu.Lock()
		delete(callContext, callID)
		contextMu.Unlock()
		close(resultChan)
	}()

	// 构造调用指令，与Python版 call_msg 格式完全一致
	callMsg := WsMessage{
		Cmd:       "invoke",
		Group:     group,
		Interface: iface,
		Params:    params,
		CallID:    callID,
		From:      "http/local/websocket", // 与Python版一致
	}
	if err := targetClient.WriteJSON(callMsg); err != nil {
		log.Printf("发送指令到客户端 %s 失败: %v", targetClient.RemoteAddr(), err)
		return &InvokeResult{
			Success: false,
			Error:   fmt.Sprintf("调用失败: 发送指令失败 - %v", err),
			CallID:  callID,
		}
	}

	// 等待客户端响应，10秒超时（与Python版 asyncio.wait_for 逻辑一致）
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

// handleWsClient 处理WebSocket客户端连接：根路径路由，与Python版 handle_client 逻辑一致
func handleWsClient(w http.ResponseWriter, r *http.Request) {
	// 升级HTTP连接为WebSocket（根路径，客户端无需加后缀）
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket升级失败: %v", err)
		// 对齐Python版，返回1006错误码，适配客户端异常处理
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	defer conn.Close()

	clientAddr := conn.RemoteAddr().String()
	log.Printf("新WebSocket客户端连接: %s", clientAddr)

	// 初始化客户端元数据，与Python版 CLIENT_META 对齐
	metaMu.Lock()
	clientMeta[conn] = &ClientMeta{
		Groups:     make(map[string]struct{}),
		Interfaces: make(map[string]map[string]struct{}),
	}
	metaMu.Unlock()
	defer cleanupClient(conn) // 连接关闭时清理资源，与Python版 finally 逻辑一致

	// 循环读取客户端消息，与Python版 async for message 逻辑一致
	for {
		var msg WsMessage
		if err := conn.ReadJSON(&msg); err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				log.Printf("WebSocket客户端正常断开: %s", clientAddr)
			} else {
				log.Printf("WebSocket读取消息失败: %s, 错误: %v", clientAddr, err)
			}
			return
		}
		// 分发处理消息，与Python版 dispatch_message 逻辑一致
		if err := dispatchWsMessage(conn, &msg); err != nil {
			log.Printf("处理客户端 %s 消息失败: %v", clientAddr, err)
			_ = sendWsResponse(conn, &InvokeResult{
				Success: false,
				Error:   fmt.Sprintf("处理消息失败: %v", err),
			})
		}
	}
}

// dispatchWsMessage 消息分发：完全复刻Python版 dispatch_message 所有指令逻辑
func dispatchWsMessage(conn *websocket.Conn, msg *WsMessage) error {
	// 1. 处理客户端返回的执行结果（无cmd，含call_id和success，与Python版一致）
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

	// 2. 处理注册指令（cmd=register），与Python版一致
	if msg.Cmd == "register" {
		if msg.Group == "" || msg.Interface == "" {
			return sendWsResponse(conn, &InvokeResult{
				Success: false,
				Error:   "缺少group或interface参数",
			})
		}
		return registerInterface(conn, msg.Group, msg.Interface)
	}

	// 3. 处理WebSocket客户端发起的调用（cmd=call），与Python版一致
	if msg.Cmd == "call" {
		result := invokeInterface(msg.Group, msg.Interface, msg.Params)
		result.CallID = msg.CallID
		return sendWsResponse(conn, result)
	}

	// 4. 处理心跳检测（cmd=heartbeat），与Python版一致（pong响应）
	if msg.Cmd == "heartbeat" {
		return sendWsResponse(conn, &InvokeResult{
			Success: true,
			Data:    "pong",
		})
	}

	// 5. 未知指令，与Python版错误响应一致
	return sendWsResponse(conn, &InvokeResult{
		Success: false,
		Error:   fmt.Sprintf("未知指令: %s", msg.Cmd),
	})
}

// registerInterface 接口注册：完全复刻Python版 register_interface 逻辑
func registerInterface(conn *websocket.Conn, group, iface string) error {
	registryMu.Lock()
	defer registryMu.Unlock()

	// 初始化分组和接口映射，与Python版 INTERFACE_REGISTRY 对齐
	if _, exist := interfaceRegistry[group]; !exist {
		interfaceRegistry[group] = make(map[string]*sync.Map)
	}
	if _, exist := interfaceRegistry[group][iface]; !exist {
		interfaceRegistry[group][iface] = &sync.Map{}
	}

	// 将客户端加入接口注册表，与Python版一致
	clientMap := interfaceRegistry[group][iface]
	clientMap.Store(conn, struct{}{})

	// 更新客户端元数据，与Python版 CLIENT_META 对齐
	metaMu.Lock()
	meta := clientMeta[conn]
	meta.Groups[group] = struct{}{}
	if _, exist := meta.Interfaces[group]; !exist {
		meta.Interfaces[group] = make(map[string]struct{})
	}
	meta.Interfaces[group][iface] = struct{}{}
	metaMu.Unlock()

	// 统计当前接口客户端数，与Python版日志和返回信息一致
	clientCount := 0
	clientMap.Range(func(_, _ interface{}) bool {
		clientCount++
		return true
	})

	log.Printf("客户端 %s 注册接口: %s.%s (当前该接口总客户端数: %d)", conn.RemoteAddr(), group, iface, clientCount)
	return sendWsResponse(conn, &InvokeResult{
		Success: true,
		Data:    fmt.Sprintf("注册 %s.%s 成功，当前该接口在线客户端数: %d", group, iface, clientCount),
	})
}

// sendWsResponse 统一WebSocket响应：与Python版 send_response 格式完全一致
func sendWsResponse(conn *websocket.Conn, result *InvokeResult) error {
	return conn.WriteJSON(result)
}

// cleanupClient 客户端资源清理：完全复刻Python版 cleanup_client 逻辑
func cleanupClient(conn *websocket.Conn) {
	clientAddr := conn.RemoteAddr().String()
	metaMu.RLock()
	meta, exist := clientMeta[conn]
	metaMu.RUnlock()

	if !exist {
		return
	}

	// 从接口注册表移除客户端，与Python版一致
	registryMu.Lock()
	for group, ifaces := range meta.Interfaces {
		for iface := range ifaces {
			if groupMap, gExist := interfaceRegistry[group]; gExist {
				if clientMap, iExist := groupMap[iface]; iExist {
					clientMap.Delete(conn)
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

	// 删除客户端元数据，与Python版一致
	metaMu.Lock()
	delete(clientMeta, conn)
	metaMu.Unlock()

	// 清理调用上下文，与Python版一致
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

// httpRpcCall HTTP调用接口：与Python版 /rpc/call 逻辑一致，400错误码对齐
func httpRpcCall(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

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

	result := invokeInterface(req.Group, req.Interface, req.Params)
	if !result.Success {
		w.WriteHeader(http.StatusBadRequest) // 与Python版 HTTPException(400) 对齐
	}
	_ = json.NewEncoder(w).Encode(result)
}

// httpRpcList 接口查询：与Python版 /rpc/list 逻辑一致，返回格式完全对齐
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
		Message: "已注册接口列表（含客户端信息）", // 与Python版返回信息一致
	})
}

// startServices 启动服务：WebSocket+HTTP双服务，与Python版 start_services 逻辑一致
func startServices() {
	// 注册路由：WebSocket根路径 / 、HTTP /rpc/call /rpc/list（与Python版完全一致）
	http.HandleFunc("/", handleWsClient)
	http.HandleFunc("POST /rpc/call", httpRpcCall)
	http.HandleFunc("GET /rpc/list", httpRpcList)

	// 启动WebSocket服务（与Python版一致，独立协程）
	go func() {
		wsAddr := fmt.Sprintf("0.0.0.0:%d", wsPort)
		log.Printf("WebSocket服务启动成功，监听地址: %s（根路径，客户端直接连接）", wsAddr)
		if err := http.ListenAndServe(wsAddr, nil); err != nil && err != http.ErrServerClosed {
			log.Fatalf("WebSocket服务启动失败: %v", err)
		}
	}()

	// 启动HTTP服务（与Python版一致，独立协程）
	go func() {
		httpAddr := fmt.Sprintf("0.0.0.0:%d", httpPort)
		log.Printf("HTTP服务启动成功，监听地址: %s", httpAddr)
		if err := http.ListenAndServe(httpAddr, nil); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP服务启动失败: %v", err)
		}
	}()

	// 优雅退出：监听系统信号，与Python版一致（清理资源）
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("开始优雅关闭服务...")

	// 关闭所有客户端连接，与Python版一致
	metaMu.RLock()
	for conn := range clientMeta {
		// 发送带描述的关闭帧
		closeMsg := websocket.FormatCloseMessage(1006, "服务端优雅关闭")
		_ = conn.WriteControl(websocket.CloseMessage, closeMsg, time.Now().Add(1*time.Second))
		// 关闭连接
		_ = conn.Close()
	}
	metaMu.RUnlock()

	log.Println("服务已优雅关闭，所有资源清理完成")
}

func main() {
	// 启动服务，无额外配置，与Python版运行方式一致
	startServices()
}