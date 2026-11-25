package conf

import (
	"encoding/json"
	"github.com/name5566/leaf/log"
	"io/ioutil"
)

var Server struct {
	LogLevel        string
	LogPath         string
	WSAddr          string
	CertFile        string
	KeyFile         string
	TCPAddr         string
	MaxConnNum      int
	ConsolePort     int
	ProfilePath     string
	ConfPath        string
	MgodbAddr       string
	GameMgoConnNum  int
	LoginMgoConnNum int
	OssLogAddr      string
}

// confPath 可由测试覆盖，指向仓库内的配置文件，保证用例不依赖工作目录。
var confPath = "conf/server.json"

func init() {
	data, err := ioutil.ReadFile(confPath)
	if err != nil {
		// 配置文件缺失时不再直接终止进程：保留零值默认配置，
		// 让单元测试等无配置场景可以独立运行；生产部署始终带有配置文件，行为不变。
		log.Error("config file not found, use zero-value defaults:", err.Error())
		return
	}
	err = json.Unmarshal(data, &Server)
	if err != nil {
		log.Fatal("%v", err)
	}
}
