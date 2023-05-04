package client

import "fmt"

type defaultLogger struct {
}

func (l *defaultLogger) Info(msg, id, name string) {
	fmt.Printf("msg=%s id=%s name=%s\n", msg, id, name)
}

func (l *defaultLogger) Error(err error) {
	fmt.Printf("error=%s\n", err)
}

type Logger interface {
	//Info logging info
	// msg: message for logging
	// id: scrape id
	// name: register name
	Info(msg, id, name string)

	//Error logging error
	Error(err error)
}
