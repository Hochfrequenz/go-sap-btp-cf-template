package foo

import "fmt"

type Foo struct {
}

func (foo Foo) SayHello(name string) string {
	return fmt.Sprintf("hello %s", name)
}
