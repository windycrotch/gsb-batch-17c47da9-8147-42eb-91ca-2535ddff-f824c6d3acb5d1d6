package rabbitmqx

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/wagslane/go-rabbitmq"
)

func rabbitMQTestURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("RABBITMQ_URL")
	if url == "" {
		t.Skip("skip external RabbitMQ test; set RABBITMQ_URL to enable it")
	}
	return url
}

func TestNewDelayedQueue(t *testing.T) {
	url := rabbitMQTestURL(t)
	config := NewConfigByMap(map[string]interface{}{
		"mqurl":      url,
		"exchange":   "test.deleayed.ex",
		"routingkey": "test.deleayed.rk",
		"queue":      "test.deleayed.queue",
	})
	d, err := NewDelayedQueueConsumer(config)
	if err != nil {
		t.Fatal(err)
	}

	d.Consume(func(d rabbitmq.Delivery) (action rabbitmq.Action) {
		fmt.Printf("Body: %v, Time: %v \n", d.Body, time.Now().Unix())
		return rabbitmq.Ack
	})

	time.Sleep(20 * time.Second)
}

func TestPublishDelayed(t *testing.T) {
	url := rabbitMQTestURL(t)
	p, err := NewPublisher(url)
	if err != nil {
		t.Fatal(err)
	}
	p.SetExchange("test.deleayed.ex")
	p.SetRoutingKey([]string{"test.deleayed.rk"})

	t.Logf("Send: %v", time.Now().Unix())
	if err := p.PublishDelayed([]byte("11111"), 1000*10); err != nil {
		t.Error(err)
	}
}
