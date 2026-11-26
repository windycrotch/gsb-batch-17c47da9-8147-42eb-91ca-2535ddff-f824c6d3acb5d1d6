package idgen

import (
	"sync"

	"github.com/bwmarrin/snowflake"
)

var (
	_node  *snowflake.Node
	nodeMu sync.RWMutex
)

// InitNode 初始化生成ID生成器
func InitNode(instanceID int64) {
	node, err := snowflake.NewNode(instanceID)
	if err != nil {
		panic(err)
	}

	nodeMu.Lock()
	_node = node
	nodeMu.Unlock()
}

func currentNode() *snowflake.Node {
	nodeMu.RLock()
	node := _node
	nodeMu.RUnlock()
	if node != nil {
		return node
	}

	nodeMu.Lock()
	defer nodeMu.Unlock()
	if _node == nil {
		node, err := snowflake.NewNode(1)
		if err != nil {
			panic(err)
		}
		_node = node
	}
	return _node
}

// GenerateInt64 创建一个int64的ID
func GenerateInt64() int64 {
	return currentNode().Generate().Int64()
}

// GenerateString 创建一个string的ID
func GenerateString() string {
	return currentNode().Generate().String()
}
