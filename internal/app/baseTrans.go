package app

import (
	"bufio"
	"fmt"
	"local-mirror/config"
	"local-mirror/pkg/utils"
	"net"
	"strings"
)

func handleConnection(conn net.Conn) {
	defer conn.Close()
	fmt.Println("Client connected:", conn.RemoteAddr())
	reader := bufio.NewReader(conn)
	for {
		data, err := reader.ReadString('\n')
		if err != nil {
			fmt.Println("Error reading from client:", err)
			break
		}
		data = strings.TrimSpace(data)
		if data == "exit" {
			fmt.Println("Client disconnected:", conn.RemoteAddr())
			break
		}
		fmt.Println("Received from client:", data)
	}
}

func baseTrans() {
	fmt.Print("mode: ", *config.Mode)
	if *config.Mode == "real" {
		listerns, err := net.Listen("tcp", ":52345")
		if err != nil {
			fmt.Println("Error starting server:", err)
			return
		}
		defer listerns.Close()
		fmt.Println("Server started on :52345")
		for {
			conn, err := listerns.Accept()
			if err != nil {
				fmt.Println("Error accepting connection:", err)
				continue
			}
			go handleConnection(conn)
		}
	} else if *config.Mode == "mirror" {
		conn, err := net.Dial("tcp", "10.8.0.9:52345")
		if err != nil {
			fmt.Println("Error connecting to server:", err)
			return
		}
		defer conn.Close()
		fmt.Println("Connected to server")
		for i := 1; i <= 9; i++ {
			if i >= 9 {
				osInfo := utils.BaseOSInfo()
				jsonStr, err := utils.StructToJson(osInfo)
				if err != nil {
					fmt.Println("Error converting struct to JSON:", err)
					break
				}
				_, err = conn.Write([]byte(jsonStr + "\n"))
				if err != nil {
					fmt.Println("Error sending to server:", err)
					break
				}
			} else {
				msg := fmt.Sprintf("hello form client %d\n", i)
				_, err := conn.Write([]byte(msg))
				if err != nil {
					fmt.Println("Error sending to server:", err)
					break
				}
			}
		}
		_, err = conn.Write([]byte("exit\n"))
		if err != nil {
			fmt.Println("Error sending exit to server:", err)
		}
		fmt.Println("Sent exit to server")
	}
}
