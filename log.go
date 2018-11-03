package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
)

type handler struct {
}

func (h *handler) ProcessPacket(saddr net.Addr, data []byte) {
	fmt.Printf("%s", string(data))
}

// startlogger starts a udp server that can listen to logs forwarded by svlogd
// add u127.0.0.1:port in your log config
func startlogger(ctx context.Context, port int) {

	laddr := fmt.Sprintf(":%d", port)
	fmt.Printf("listening on: %s\n", laddr)
	handler := &handler{}
	udpServer := NewUDPServer(laddr, handler)
	if err := udpServer.ListenAndServe(ctx); err != nil {
		log.Fatal(err)
	}
}

// PacketProcessor is the interface function that runs when a packet is recieved
type PacketProcessor interface {
	ProcessPacket(net.Addr, []byte)
}

// UDPServer uses a queue to push pop messages from the queue
// udpserver inspired by jtblin
// https://gist.github.com/jtblin/18df559cf14438223f93#file-udp-server-go
type UDPServer struct {
	address       string
	workers       int
	conn          net.PacketConn
	mq            messageQueue
	processor     PacketProcessor
	maxQueueSize  int
	UDPPacketSize int
	bufferPool    sync.Pool
	Packets       uint64
}

// NewUDPServer creates a new server and takes in the interface that will process
// the packets received
func NewUDPServer(address string, processor PacketProcessor) *UDPServer {

	maxQueueSize := 1000000
	UDPPacketSize := 3000

	bufferPool := sync.Pool{
		New: func() interface{} { return make([]byte, UDPPacketSize) },
	}

	return &UDPServer{
		mq:            make(messageQueue, maxQueueSize),
		address:       address,
		processor:     processor,
		maxQueueSize:  maxQueueSize,
		UDPPacketSize: UDPPacketSize,
		workers:       runtime.NumCPU(),
		bufferPool:    bufferPool,
		Packets:       0,
	}
}

// ListenAndServe begine listening
func (u *UDPServer) ListenAndServe(ctx context.Context) error {

	conn, err := net.ListenPacket("udp", u.address)
	if err != nil {
		return err
	}

	for i := 0; i < u.workers; i++ {
		go u.dequeue()
		go u.receive(conn)
	}
	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		<-ctx.Done()
		wg.Done()
	}()

	wg.Wait()
	return nil
}

type messageQueue chan message

type message struct {
	addr   net.Addr
	msg    []byte
	length int
}

func (u *UDPServer) enqueue(m message) {
	u.mq <- m
}

func (u *UDPServer) dequeue() {
	for m := range u.mq {
		u.processor.ProcessPacket(m.addr, m.msg[0:m.length])
		u.bufferPool.Put(m.msg)
	}
}

func (u *UDPServer) receive(c net.PacketConn) {
	for {
		msg := u.bufferPool.Get().([]byte)
		nbytes, addr, err := c.ReadFrom(msg[0:])
		if err != nil {
			log.Printf("Error %s", err)
			continue
		}
		u.enqueue(message{addr, msg, nbytes})
		atomic.AddUint64(&u.Packets, 1)
	}
}
