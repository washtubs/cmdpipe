package cmdpipe

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"os"
	"os/exec"
	"path"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/andrew-d/go-termutil"

	rmq "gopkg.in/adjust/rmq.v1"
)

const (
	service = "cmdpipe"
)

type Command struct {
	Name   string   `json:"name"`
	Params []string `json:"params"`
	Out    string   `json:"out"`
	In     string   `json:"in"`
	Error  string   `json:"error"`
	Exit   string   `json:"exit"`
}

type CommandConsumer struct {
	AllowedName string
}

func dialAll(command Command) (outConn, inConn, errConn, exitConn net.Conn) {
	outConn, err := net.Dial("unix", command.Out)
	if err != nil {
		log.Printf("Error dialing output %s: %s", command.Out, err.Error())
		return
	}

	inConn, err = net.Dial("unix", command.In)
	if err != nil {
		log.Printf("Error dialing input %s: %s", command.In, err.Error())
		return
	}

	errConn, err = net.Dial("unix", command.Error)
	if err != nil {
		log.Printf("Error dialing error %s: %s", command.Error, err.Error())
		return
	}

	exitConn, err = net.Dial("unix", command.Exit)
	if err != nil {
		log.Printf("Error dialing exit %s: %s", command.Exit, err.Error())
		return
	}

	return
}

func (c *CommandConsumer) Consume(delivery rmq.Delivery) {
	var command Command
	fmt.Println(delivery.Payload())
	err := json.Unmarshal([]byte(delivery.Payload()), &command)
	if err != nil {
		log.Printf("Error %s", err.Error())
		log.Printf("Problem unmarshalling payload %s", delivery.Payload())
		return
	}

	outConn, inConn, errConn, exitConn := dialAll(command)
	defer outConn.Close()
	defer inConn.Close()
	defer errConn.Close()
	defer exitConn.Close()

	fmt.Printf("Got command [%s]\n", command.Name)
	if command.Name != c.AllowedName {
		log.Printf("Rejecting [%s]\n", command.Name)
		delivery.Reject()
		return
	}

	fmt.Printf("Dialed: %s %s %s %s\n", command.Out, command.In, command.Error, command.Exit)

	cmd := exec.Command(c.AllowedName, command.Params...)
	cmd.Stdout = outConn
	cmd.Stdin = inConn
	cmd.Stderr = errConn

	fmt.Printf("Command started\n")
	err = cmd.Run()
	exitCode := -1
	if err != nil {
		exitErr, isExitErr := err.(*exec.ExitError)
		if !isExitErr {
			log.Printf("Non exit-error running command: %s\n", err.Error())
		} else {
			if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
				exitCode = status.ExitStatus()
			}
		}
	} else {
		exitCode = 0
	}
	log.Println("Writing to exitConn <- " + strconv.Itoa(exitCode))
	io.WriteString(exitConn, strconv.Itoa(exitCode))

	fmt.Printf("Command completed\n")

}

func getTemp() string {
	tmp := os.Getenv("CMDPIPE_TMP_DIR")
	if tmp == "" {
		tmp = "/tmp"
	}
	return tmp
}

func getQueueName(commandName string) string {
	return "command:" + commandName
}

func Receive() {
	if len(os.Args) <= 1 {
		log.Printf("Need an argument to signify the allowed command")
		return
	}
	commandName := os.Args[1]

	conn := rmq.OpenConnection(service, "unix", path.Join(getTemp(), "redis.sock"), 1)
	defer conn.Close()
	queue := conn.OpenQueue(getQueueName(commandName))
	queue.StartConsuming(10, 400*time.Millisecond)

	queue.AddConsumer("command consumer", &CommandConsumer{commandName})
	select {}
}

func genCommandPipeSocket(pipeType string) string {
	return path.Join(getTemp(), "cmdpipe-"+RandStringBytesMaskImprSrc(6)+"-"+pipeType)
}

func Send() int {
	if len(os.Args) <= 1 {
		log.Printf("Need a command name argument")
		return 1
	}
	commandName := os.Args[1]

	conn := rmq.OpenConnection(service, "unix", path.Join(getTemp(), "redis.sock"), 1)
	defer conn.Close()
	queue := conn.OpenQueue(getQueueName(commandName))

	outSock := genCommandPipeSocket("out")
	defer os.Remove(outSock)
	outConn, err := net.Listen("unix", outSock)
	if err != nil {
		panic(err.Error())
	}

	errSock := genCommandPipeSocket("err")
	defer os.Remove(errSock)
	errConn, err := net.Listen("unix", errSock)
	if err != nil {
		panic(err.Error())
	}

	inSock := genCommandPipeSocket("in")
	defer os.Remove(inSock)
	inConn, err := net.Listen("unix", inSock)
	if err != nil {
		panic(err.Error())
	}

	exitSock := genCommandPipeSocket("exit")
	defer os.Remove(exitSock)
	exitConn, err := net.Listen("unix", exitSock)
	if err != nil {
		panic(err.Error())
	}

	var wg sync.WaitGroup
	wg.Add(3)

	go func(outConn net.Listener) {
		defer outConn.Close()
		fd, err := outConn.Accept()
		if err != nil {
			log.Printf("err opening output socket: %s\n", err.Error())
		}

		io.Copy(os.Stdout, fd)

		wg.Done()
	}(outConn)

	go func(errConn net.Listener) {
		defer errConn.Close()
		fd, err := errConn.Accept()
		if err != nil {
			log.Printf("err opening error socket: %s\n", err.Error())
		}

		io.Copy(os.Stderr, fd)

		wg.Done()
	}(errConn)

	go func(inConn net.Listener) {
		defer inConn.Close()
		fd, err := inConn.Accept()
		if err != nil {
			log.Printf("err opening input socket: %s\n", err.Error())
		}

		if !termutil.Isatty(os.Stdin.Fd()) {
			io.Copy(fd, os.Stdin)
		}
		fd.Close()

		wg.Done()
	}(inConn)

	exit := make(chan int)
	go func(exitConn net.Listener) {
		defer exitConn.Close()
		fd, err := exitConn.Accept()
		if err != nil {
			log.Printf("err opening exit socket: %s\n", err.Error())
		}

		buf, err := ioutil.ReadAll(fd)
		if err != nil {
			log.Printf("err reading exit socket: %s\n", err.Error())
		}

		exitCode, err := strconv.Atoi(string(buf))
		if err != nil {
			log.Printf("Error converting exit code: %s: %s", string(buf), err.Error())
			exitCode = -1
		}

		exit <- exitCode
	}(exitConn)

	params := []string{}
	if len(os.Args) > 2 {
		params = os.Args[2:]
	}
	bs, err := json.Marshal(Command{
		Name:   commandName,
		Params: params,
		Out:    outSock,
		In:     inSock,
		Error:  errSock,
		Exit:   exitSock,
	})
	if err != nil {
		panic(err.Error())
	}
	queue.PublishBytes(bs)

	wg.Wait()

	return <-exit
}
