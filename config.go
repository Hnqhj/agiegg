package main

import (
	"bufio"
	"strings"
)

// Conf 便携包内的运行时配置（无鉴权风险的自持密钥，随包分发）。
type Conf struct {
	WBKey  string
	WBPort string
}

// loadConf 从内嵌的 embedded/agiegg.env 读取密钥与端口。
func loadConf() Conf {
	c := Conf{WBPort: "7863"}
	f, err := embeddedFS.Open("embedded/agiegg.env")
	if err != nil {
		return c
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		switch k {
		case "WB_API_KEY":
			c.WBKey = v
		case "WB_PORT":
			if v != "" {
				c.WBPort = v
			}
		}
	}
	return c
}
