package gev

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	at "sync/atomic"
	"time"

	"github.com/Allenxuxu/gev/eventloop"
	"github.com/Allenxuxu/gev/log"
	"github.com/Allenxuxu/gev/poller"
	"github.com/Allenxuxu/ringbuffer"
	"github.com/Allenxuxu/toolkit/sync/atomic"
	"github.com/RussellLuo/timingwheel"
	"golang.org/x/sys/unix"
)

type CallBack interface {
	OnMessage(c *Connection, ctx interface{}, data []byte) interface{}
	OnClose(c *Connection)
}

// Connection TCP 连接
type Connection struct {
	outBufferLen atomic.Int64
	inBufferLen  atomic.Int64
	activeTime   atomic.Int64
	fd           int
	connected    atomic.Bool
	buffer       *ringbuffer.RingBuffer
	outBuffer    *ringbuffer.RingBuffer // write buffer
	inBuffer     *ringbuffer.RingBuffer // read buffer
	callBack     CallBack
	loop         *eventloop.EventLoop
	peerAddr     string
	ctx          interface{}
	KeyValueContext

	idleTime    time.Duration
	timingWheel *timingwheel.TimingWheel
	timer       at.Value
	protocol    Protocol
}

var ErrConnectionClosed = errors.New("connection closed")

// NewConnection 创建 Connection
func NewConnection(fd int,
	loop *eventloop.EventLoop,
	sa unix.Sockaddr,
	protocol Protocol,
	tw *timingwheel.TimingWheel,
	idleTime time.Duration,
	callBack CallBack) *Connection {
	conn := &Connection{
		fd:          fd,
		peerAddr:    sockAddrToString(sa),
		outBuffer:   ringbuffer.GetFromPool(),
		inBuffer:    ringbuffer.GetFromPool(),
		callBack:    callBack,
		loop:        loop,
		idleTime:    idleTime,
		timingWheel: tw,
		protocol:    protocol,
		buffer:      ringbuffer.New(0),
	}
	conn.connected.Set(true)

	if conn.idleTime > 0 {
		_ = conn.activeTime.Swap(time.Now().Unix())
		timer := conn.timingWheel.AfterFunc(conn.idleTime, conn.closeTimeoutConn())
		conn.timer.Store(timer)
	}

	return conn
}

func (c *Connection) UserBuffer() *[]byte {
	return c.loop.UserBuffer
}

func (c *Connection) closeTimeoutConn() func() {
	return func() {
		now := time.Now()
		intervals := now.Sub(time.Unix(c.activeTime.Get(), 0))
		if intervals >= c.idleTime {
			_ = c.Close()
		} else {
			if c.connected.Get() {
				timer := c.timingWheel.AfterFunc(c.idleTime-intervals, c.closeTimeoutConn())
				c.timer.Store(timer)
			}
		}
	}
}

// Context 获取 Context
func (c *Connection) Context() interface{} {
	return c.ctx
}

// SetContext 设置 Context
func (c *Connection) SetContext(ctx interface{}) {
	c.ctx = ctx
}

// PeerAddr 获取客户端地址信息
func (c *Connection) PeerAddr() string {
	return c.peerAddr
}

// Connected 是否已连接
func (c *Connection) Connected() bool {
	return c.connected.Get()
}

// Send 用来在非 loop 协程发送
func (c *Connection) Send(data interface{}, opts ...ConnectionOption) error {
	if !c.connected.Get() {
		return ErrConnectionClosed
	}

	opt := ConnectionOptions{}
	for _, o := range opts {
		o(&opt)
	}

	c.loop.QueueInLoop(func() {
		if c.connected.Get() {
			c.sendInLoop(c.protocol.Packet(c, data))

			if opt.sendInLoopFinish != nil {
				opt.sendInLoopFinish(data)
			}
		}
	})
	return nil
}

// Close 关闭连接
func (c *Connection) Close() error {
	if !c.connected.Get() {
		return ErrConnectionClosed
	}

	c.loop.QueueInLoop(func() {
		c.handleClose(c.fd)
	})
	return nil
}

// ShutdownWrite 关闭可写端，等待读取完接收缓冲区所有数据
func (c *Connection) ShutdownWrite() error {
	return unix.Shutdown(c.fd, unix.SHUT_WR)
}

// ReadBufferLength read buffer 当前积压的数据长度
func (c *Connection) ReadBufferLength() int64 {
	return c.inBufferLen.Get()
}

// WriteBufferLength write buffer 当前积压的数据长度
func (c *Connection) WriteBufferLength() int64 {
	return c.outBufferLen.Get()
}

// HandleEvent 内部使用，event loop 回调
func (c *Connection) HandleEvent(fd int, events poller.Event) {
	if c.idleTime > 0 {
		_ = c.activeTime.Swap(time.Now().Unix())
	}

	// 处理错误事件, 关闭连接
	if events&poller.EventErr != 0 {
		c.handleClose(fd)
		return
	}

	// 处理写事件
	if !c.outBuffer.IsEmpty() {
		if events&poller.EventWrite != 0 {
			// if return true, it means closed
			if c.handleWrite(fd) {
				return
			}

			if c.outBuffer.IsEmpty() {
				c.outBuffer.Reset()
			}
		}
	} else if events&poller.EventRead != 0 {
		// 处理读事件
		// if return true, it means closed
		if c.handleRead(fd) {
			return
		}

		if c.inBuffer.IsEmpty() {
			c.inBuffer.Reset()
		}
	}

	c.inBufferLen.Swap(int64(c.inBuffer.Length()))
	c.outBufferLen.Swap(int64(c.outBuffer.Length()))
}

// 协议解码 UnPacket -> 协议处理 OnMessage -> 返回的数据编码 Packet
// @param tmpBuffer 临时 buffer, 用于存储返回的数据
// @param buffer 已经读取好的 buffer
func (c *Connection) handlerProtocol(tmpBuffer *[]byte, buffer *ringbuffer.RingBuffer) {
	ctx, receivedData := c.protocol.UnPacket(c, buffer)
	for ctx != nil || len(receivedData) != 0 {
		sendData := c.callBack.OnMessage(c, ctx, receivedData)
		if sendData != nil {
			*tmpBuffer = append(*tmpBuffer, c.protocol.Packet(c, sendData)...)
		}
		// 如果有多个数据包，继续解析处理
		ctx, receivedData = c.protocol.UnPacket(c, buffer)
	}
}

// 处理读事件
func (c *Connection) handleRead(fd int) (closed bool) {
	// TODO 避免这次内存拷贝
	// 将数据读取到 buf
	buf := c.loop.PacketBuf()
	n, err := unix.Read(c.fd, buf)
	if n == 0 || err != nil {
		if err != unix.EAGAIN {
			c.handleClose(fd)
			closed = true
		}
		return
	}

	// read buffer 读完了
	if c.inBuffer.IsEmpty() {
		// 将 buf 中的数据写入 buffer
		c.buffer.WithData(buf[:n])
		buf = buf[n:n]
		// 协议解码 UnPacket -> 协议处理 OnMessage -> 返回的数据编码 Packet
		c.handlerProtocol(&buf, c.buffer)

		if !c.buffer.IsEmpty() {
			first, _ := c.buffer.PeekAll()
			_, _ = c.inBuffer.Write(first)
		}
	} else { // read buffer 还有数据
		// 将 buf 中的数据写入 read buffer
		_, _ = c.inBuffer.Write(buf[:n])
		buf = buf[:0]
		// 协议解码 UnPacket -> 协议处理 OnMessage -> 返回的数据编码 Packet
		c.handlerProtocol(&buf, c.inBuffer)
	}

	// 将返回的数据发送写给客户端 fd
	if len(buf) != 0 {
		closed = c.sendInLoop(buf)
	}
	return
}

func (c *Connection) handleWrite(fd int) (closed bool) {
	first, end := c.outBuffer.PeekAll()
	n, err := unix.Write(c.fd, first)
	if err != nil {
		if err == unix.EAGAIN {
			return
		}
		c.handleClose(fd)
		closed = true
		return
	}
	c.outBuffer.Retrieve(n)

	if n == len(first) && len(end) > 0 {
		n, err = unix.Write(c.fd, end)
		if err != nil {
			if err == unix.EAGAIN {
				return
			}
			c.handleClose(fd)
			closed = true
			return
		}
		c.outBuffer.Retrieve(n)
	}

	if c.outBuffer.IsEmpty() {
		if err := c.loop.EnableRead(fd); err != nil {
			log.Error("[enableRead]", err)
		}
	}

	return
}

func (c *Connection) handleClose(fd int) {
	if c.connected.Get() {
		c.connected.Set(false)
		c.loop.DeleteFdInLoop(fd)
		c.callBack.OnClose(c)
		if err := unix.Close(fd); err != nil {
			log.Error("[close fd]", err)
		}
		ringbuffer.PutInPool(c.inBuffer)
		ringbuffer.PutInPool(c.outBuffer)
		if v := c.timer.Load(); v != nil {
			timer := v.(*timingwheel.Timer)
			timer.Stop()
		}
	}
}

// 将返回的数据放到 outBuffer 中
func (c *Connection) sendInLoop(data []byte) (closed bool) {
	// 如果 outBuffer 为空, 直接将数据放到 outBuffer 中
	if !c.outBuffer.IsEmpty() {
		_, _ = c.outBuffer.Write(data)
	} else {
		// 如果 outBuffer 不为空, 尝试写入数据
		n, err := unix.Write(c.fd, data)
		if err != nil && err != unix.EAGAIN {
			c.handleClose(c.fd)
			closed = true
			return
		}

		// 把没写完的数据放到 outBuffer 中
		if n <= 0 {
			_, _ = c.outBuffer.Write(data)
		} else if n < len(data) {
			_, _ = c.outBuffer.Write(data[n:])
		}

		// 如果 outBuffer 不为空, 则注册写事件
		if !c.outBuffer.IsEmpty() {
			_ = c.loop.EnableReadWrite(c.fd)
		}
	}

	return
}

func sockAddrToString(sa unix.Sockaddr) string {
	switch sa := (sa).(type) {
	case *unix.SockaddrInet4:
		return net.JoinHostPort(net.IP(sa.Addr[:]).String(), strconv.Itoa(sa.Port))
	case *unix.SockaddrInet6:
		return net.JoinHostPort(net.IP(sa.Addr[:]).String(), strconv.Itoa(sa.Port))
	default:
		return fmt.Sprintf("(unknown - %T)", sa)
	}
}
