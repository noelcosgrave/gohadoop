package ipc 

import (
  "errors" 
  "fmt" 
  "bytes"
  "log"
  "net"
  "strconv"
  "os/user"
  "code.google.com/p/goprotobuf/proto"
  "github.com/nu7hatch/gouuid"
  "github.com/gohadooprpc"
  "github.com/gohadooprpc/hadoop_common"
)

type Client struct {
  ClientId *uuid.UUID
  Server string
  Port int
  TCPNoDelay bool
}

type connection struct {
  con *net.TCPConn 
}

type call struct {
  callId uint32  // TODO HADOOP-9944 
  procedure proto.Message
  request proto.Message
  response proto.Message
  err *error
  retryCount int32
}

func (c *Client) String() (string) {
  buf := bytes.NewBufferString("")
  fmt.Fprint(buf, "<clientId:", c.ClientId)
  fmt.Fprint(buf, ", server:", getServerAddr(c)) 
  fmt.Fprint(buf, ">");
  return buf.String()
}

func (c *Client) Call (rpc proto.Message, rpcRequest proto.Message, rpcResponse proto.Message) (error) {
  // Get connection to server
  log.Println("Connecting...", c)
  conn, err := getConnection(c)
  if err != nil {
    return err
  }
  
  // Create call and send request
  rpcCall := call{callId: 0, procedure: rpc, request: rpcRequest, response: rpcResponse}
  err = sendRequest(c, conn, &rpcCall)
  if (err != nil) {
    log.Fatal("sendRequest", err)
    return err
  }

  // Read & return response
  err = c.readResponse(conn, &rpcCall)
  return err
}

func getServerAddr (c *Client) (string) {
  return net.JoinHostPort(c.Server, strconv.Itoa(c.Port))
}

func getConnection (c *Client) (*connection, error) {
  con, err := setupConnection(c)
  writeConnectionHeader(con)
  writeConnectionContext(c, con)
  return con, err
}

func setupConnection (c *Client) (*connection, error) {
  addr, _ := net.ResolveTCPAddr("tcp", getServerAddr(c))
  tcpConn, err := net.DialTCP("tcp", nil, addr) 
  if err != nil {
    log.Println("error: ", err)
    return nil, err
  } else {
    log.Println("Successfully connected ", c)
  }

  // TODO: Ping thread

  // Set tcp no-delay
  tcpConn.SetNoDelay(c.TCPNoDelay)

  return &connection{tcpConn}, nil 
}

func writeConnectionHeader (conn *connection) (error) {
  // RPC_HEADER
  if _, err := conn.con.Write(gohadooprpc.RPC_HEADER); err != nil {
    log.Fatal("conn.Write gohadooprpc.RPC_HEADER", err)
    return err
  } 

  // RPC_VERSION
  if _, err := conn.con.Write(gohadooprpc.VERSION); err != nil {
    log.Fatal("conn.Write gohadooprpc.VERSION", err)
    return err
  } 

  // RPC_SERVICE_CLASS
  if serviceClass, err := gohadooprpc.ConvertFixedToBytes(gohadooprpc.RPC_SERVICE_CLASS); err != nil {
    log.Fatal("binary.Write", err)
    return err
  } else if _, err := conn.con.Write(serviceClass); err != nil {
    log.Fatal("conn.Write RPC_SERVICE_CLASS", err)
    return err
  }

  // AuthProtocol
  if authProtocol, err := gohadooprpc.ConvertFixedToBytes(gohadooprpc.AUTH_PROTOCOL_NONE); err != nil {
    log.Fatal("WTF AUTH_PROTOCOL_NONE", err)
    return err
  } else if _, err := conn.con.Write(authProtocol); err != nil {
    log.Fatal("conn.Write gohadooprpc.AUTH_PROTOCOL_NONE", err)
    return err
  }

  return nil 
}

func writeConnectionContext (c *Client, conn *connection) (error) {
  // Figure the current user-name
  var username string
  if user, err := user.Current(); err != nil {
    log.Fatal("user.Current", err)
    return err
  } else {
    username = user.Username
  }
  userProto := hadoop_common.UserInformationProto{EffectiveUser: nil, RealUser: &username}

  // Create hadoop_common.IpcConnectionContextProto
  protocolName := "org.apache.hadoop.yarn.api.ApplicationClientProtocolPB"
  ipcCtxProto := hadoop_common.IpcConnectionContextProto{UserInfo: &userProto, Protocol: &protocolName}
  
  // Create RpcRequestHeaderProto
  var callId uint32 = 4294967293 // TODO: HADOOP-9944
  var clientId [16]byte = [16]byte(*c.ClientId)
  rpcReqHeaderProto := hadoop_common.RpcRequestHeaderProto {RpcKind: &gohadooprpc.RPC_PROTOCOL_BUFFFER, RpcOp: &gohadooprpc.RPC_FINAL_PACKET, CallId: &callId, ClientId: clientId[0:16], RetryCount: &gohadooprpc.RPC_DEFAULT_RETRY_COUNT}

  rpcReqHeaderProtoBytes, err := proto.Marshal(&rpcReqHeaderProto)
  if err != nil {
    log.Fatal("proto.Marshal(&rpcReqHeaderProto)", err)
    return err
  }

  ipcCtxProtoBytes, _ := proto.Marshal(&ipcCtxProto)
  if err != nil {
    log.Fatal("proto.Marshal(&ipcCtxProto)", err)
    return err
  }

  totalLength := len(rpcReqHeaderProtoBytes) + sizeVarint(len(rpcReqHeaderProtoBytes)) + len(ipcCtxProtoBytes) + sizeVarint(len(ipcCtxProtoBytes))
  var tLen int32 = int32(totalLength)
  if totalLengthBytes, err := gohadooprpc.ConvertFixedToBytes(tLen); err != nil {
    log.Fatal("ConvertFixedToBytes(totalLength)", err)
    return err
  } else if _, err := conn.con.Write(totalLengthBytes); err != nil {
    log.Fatal("conn.con.Write(totalLengthBytes)", err)
    return err
  }

  if err := writeDelimitedBytes(conn, rpcReqHeaderProtoBytes); err != nil {
    log.Fatal("writeDelimitedBytes(conn, rpcReqHeaderProtoBytes)", err)
    return err
  }
  if err := writeDelimitedBytes(conn, ipcCtxProtoBytes); err != nil {
    log.Fatal("writeDelimitedBytes(conn, ipcCtxProtoBytes)", err)
    return err
  }

  return nil
}

func sizeVarint(x int) (n int) {
  for {
    n++
    x >>= 7
    if x == 0 {
      break
    }
  }
  return n
}

func sendRequest (c *Client, conn *connection, rpcCall *call) (error) {
  
  // 0. RpcRequestHeaderProto
  var clientId [16]byte = [16]byte(*c.ClientId)
  rpcReqHeaderProto := hadoop_common.RpcRequestHeaderProto {RpcKind: &gohadooprpc.RPC_PROTOCOL_BUFFFER, RpcOp: &gohadooprpc.RPC_FINAL_PACKET, CallId: &rpcCall.callId, ClientId: clientId[0:16], RetryCount: &rpcCall.retryCount}
  rpcReqHeaderProtoBytes, err := proto.Marshal(&rpcReqHeaderProto)
  if (err != nil) {
    log.Fatal("proto.Marshal(&rpcReqHeaderProto)", err)
    return err
  }

  // 1. RequestHeaderProto
  requestHeaderProto := rpcCall.procedure
  requestHeaderProtoBytes, err := proto.Marshal(requestHeaderProto)
  if (err != nil) {
    log.Fatal("proto.Marshal(&requestHeaderProto)", err)
    return err
  }

  // 2. Param
  paramProto := rpcCall.request
  paramProtoBytes, err := proto.Marshal(paramProto)
  if (err != nil) {
    log.Fatal("proto.Marshal(&paramProto)", err)
    return err
  }

  totalLength := len(rpcReqHeaderProtoBytes) + sizeVarint(len(rpcReqHeaderProtoBytes)) + len(requestHeaderProtoBytes) + sizeVarint(len(requestHeaderProtoBytes)) + len(paramProtoBytes) + sizeVarint(len(paramProtoBytes))
  var tLen int32 = int32(totalLength)
  if totalLengthBytes, err := gohadooprpc.ConvertFixedToBytes(tLen); err != nil {
    log.Fatal("ConvertFixedToBytes(totalLength)", err)
    return err
  } else {
    if _, err := conn.con.Write(totalLengthBytes); err != nil {
    log.Fatal("conn.con.Write(totalLengthBytes)", err)
    return err
  }
 } 

  if err := writeDelimitedBytes(conn, rpcReqHeaderProtoBytes); err != nil {
    log.Fatal("writeDelimitedBytes(conn, rpcReqHeaderProtoBytes)", err)
    return err
  }
  if err := writeDelimitedBytes(conn, requestHeaderProtoBytes); err != nil {
    log.Fatal("writeDelimitedBytes(conn, requestHeaderProtoBytes)", err)
    return err
  }
  if err := writeDelimitedBytes(conn, paramProtoBytes); err != nil {
    log.Fatal("writeDelimitedBytes(conn, paramProtoBytes)", err)
    return err
  }
  return nil
}

func writeDelimitedTo (conn *connection, msg proto.Message) (error) {
  msgBytes, err := proto.Marshal(msg)
  if err != nil {
    log.Fatal("proto.Marshal(msg)", err)
    return err
  }
  return writeDelimitedBytes(conn, msgBytes)
}

func writeDelimitedBytes (conn *connection, data []byte) (error) {
  if _, err := conn.con.Write(proto.EncodeVarint(uint64(len(data)))); err != nil {
    log.Fatal("conn.con.Write(proto.EncodeVarint(uint64(len(data))))", err)
    return err
  } 
  if _, err := conn.con.Write(data); err != nil {
    log.Fatal("conn.con.Write(data)", err)
    return err
  } 

  return nil
}

func (c *Client) readResponse (conn *connection, rpcCall *call) (error) {
  // Read first 4 bytes to get total-length
  var totalLength int32 = -1;
  var totalLengthBytes [4]byte 
  if _, err := conn.con.Read(totalLengthBytes[0:4]); err != nil {
    log.Fatal("conn.con.Read(totalLengthBytes)", err)
    return err
  }

  if err := gohadooprpc.ConvertBytesToFixed(totalLengthBytes[0:4], &totalLength); err !=  nil {
    log.Fatal("gohadooprpc.ConvertBytesToFixed(totalLengthBytes, &totalLength)", err)
    return err
  }
  log.Println("totalLength = ", totalLength)

  var responseBytes []byte = make([]byte, totalLength)
  if _, err := conn.con.Read(responseBytes); err != nil {
    log.Fatal("conn.con.Read(totalLengthBytes)", err)
    return err
  }

  // Parse RpcResponseHeaderProto
  rpcResponseHeaderProto := hadoop_common.RpcResponseHeaderProto{}
  off, err := readDelimited(responseBytes[0:totalLength], &rpcResponseHeaderProto)
  if err != nil {
    log.Fatal("readDelimited(responseBytes, rpcResponseHeaderProto)", err)
    return err
  }
  log.Println("Recieved rpcResponseHeaderProto = ", rpcResponseHeaderProto)
  log.Println("XXX off = ", off)

  err = c.checkRpcHeader(&rpcResponseHeaderProto)
  if err != nil {
    log.Fatal("c.checkRpcHeader failed", err)
    return err
  }

  // Parse RpcResponseWrapper
  soff, err := readDelimited(responseBytes[off:], rpcCall.response)
  log.Println("XXX off = ", off)
  log.Println("XXX soff = ", soff)
  return err
}

func readDelimited (rawData []byte, msg proto.Message) (int, error) {
  headerLength, off := proto.DecodeVarint(rawData)
  if off == 0 {
    log.Fatal("proto.DecodeVarint(rawData) returned zero")
    return -1, nil 
  }
  err := proto.Unmarshal(rawData[off:headerLength+1], msg) // headerLength+1 for the slice
  if (err != nil) {
    log.Fatal("XXX proto.Unmarshal(rawData[off:headerLength+1]) ", err)
    return -1, err
  }
  log.Println("readDelimited ", msg)

  return off+int(headerLength), nil
}

func (c *Client) checkRpcHeader (rpcResponseHeaderProto *hadoop_common.RpcResponseHeaderProto) (error) {
  var callClientId [16]byte = [16]byte(*c.ClientId)
  var headerClientId []byte = []byte(rpcResponseHeaderProto.ClientId)
  if rpcResponseHeaderProto.ClientId != nil {
    if !bytes.Equal(callClientId[0:16], headerClientId[0:16]) {
      log.Fatal("Incorrect clientId: ", headerClientId) 
      return errors.New("Incorrect clientId")
    }
  }
  log.Println("Successfully finished checkRpcHeader")
  return nil
}

