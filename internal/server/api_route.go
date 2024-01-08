package server

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/pkg/errors"
	"github.com/samlau0508/imserver/pkg/okhttp"
	"github.com/samlau0508/imserver/pkg/oklog"
	"go.uber.org/zap"
)

// 这个主要为了模拟proxy模式。
type RouteAPI struct {
	s *Server
	oklog.Log
}

// NewRouteAPI NewRouteAPI
func NewRouteAPI(s *Server) *RouteAPI {
	return &RouteAPI{
		s: s,
	}
}

// Route Route
func (a *RouteAPI) Route(r *okhttp.OKHttp) {
	r.GET("/route", a.routeUserIMAddr)               // 获取用户所在节点的连接信息
	r.POST("/route/batch", a.routeUserIMAddrOfBatch) // 批量获取用户所在节点的连接信息
}

// 路由用户的IM连接地址
func (a *RouteAPI) routeUserIMAddr(c *okhttp.Context) {
	c.JSON(http.StatusOK, gin.H{
		"tcp_addr": a.s.opts.External.TCPAddr,
		"ws_addr":  a.s.opts.External.WSAddr,
		"wss_addr": a.s.opts.External.WSSAddr,
	})
}

// 批量获取用户所在节点地址
func (a *RouteAPI) routeUserIMAddrOfBatch(c *okhttp.Context) {
	var uids []string
	if err := c.BindJSON(&uids); err != nil {
		a.Error("数据格式有误！", zap.Error(err))
		c.ResponseError(errors.New("数据格式有误！"))
		return
	}

	c.JSON(http.StatusOK, []userAddrResp{
		{
			UIDs:    uids,
			TCPAddr: a.s.opts.External.TCPAddr,
			WSAddr:  a.s.opts.External.WSAddr,
			WSSAddr: a.s.opts.External.WSSAddr,
		},
	})
}

type userAddrResp struct {
	TCPAddr string   `json:"tcp_addr"`
	WSAddr  string   `json:"ws_addr"`
	WSSAddr string   `json:"wss_addr"`
	UIDs    []string `json:"uids"`
}
