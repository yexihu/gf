// Copyright 2017 gf Author(https://gitee.com/johng/gf). All Rights Reserved.
//
// This Source Code Form is subject to the terms of the MIT License.
// If a copy of the MIT was not distributed with this file,
// You can obtain one at https://gitee.com/johng/gf.
// Web Server进程间通信

package ghttp

import (
    "os"
    "gitee.com/johng/gf/g/os/gproc"
    "gitee.com/johng/gf/g/os/gtime"
    "gitee.com/johng/gf/g/util/gconv"
    "gitee.com/johng/gf/g/encoding/gjson"
    "gitee.com/johng/gf/g/container/gtype"
    "gitee.com/johng/gf/g/encoding/gbinary"
)

const (
    gMSG_START       = 10
    gMSG_RESTART     = 20
    gMSG_SHUTDOWN    = 30
    gMSG_NEW_FORK    = 40
    gMSG_REMOVE_PROC = 50
    gMSG_HEARTBEAT   = 60

    gPROC_HEARTBEAT_INTERVAL    = 1000        // (毫秒)进程间心跳间隔
    gPROC_HEARTBEAT_TIMEOUT     = 30000       // (毫秒)进程间心跳超时时间，如果子进程在这段内没有接收到任何心跳，那么自动退出，防止可能出现的僵尸子进程
    //gPROC_MULTI_CHILD_CLEAR_INTERVAL   = 1000 // (毫秒)检测间隔，当存在多个子进程时(往往是重启间隔非常短且频繁造成)，需要进行清理，最终留下一个最新的子进程
    //gPROC_MULTI_CHILD_CLEAR_MIN_EXPIRE = 30000 // (毫秒)当多个子进程存在时，允许子进程进程至少运行的最小时间，超过该时间则清理
)

// 进程信号量监听消息队列
var procSignalChan  = make(chan os.Signal)

// (主子进程)在第一次创建子进程成功之后才会开始心跳检测，同理对应超时时间才会生效
var checkHeartbeat  = gtype.NewBool()

// 处理进程信号量监控以及进程间消息通信
func handleProcessMsgAndSignal() {
    go handleProcessSignal()
    if gproc.IsChild() {
        go handleChildProcessHeartbeat()
    } else {
        go handleMainProcessHeartbeat()
        //go handleMainProcessChildClear()
    }
    handleProcessMsg()
}

// 处理进程间消息
// 数据格式： 操作(8bit) | 参数(变长)
func handleProcessMsg() {
    for {
        if msg := gproc.Receive(); msg != nil {
            // 记录消息日志，用于调试
            //content := gconv.String(msg.Pid) + "=>" + gconv.String(gproc.Pid()) + ":" + fmt.Sprintf("%v\n", msg.Data)
            //fmt.Print(content)
            //gfile.PutContentsAppend("/tmp/gproc-log", content)
            act  := gbinary.DecodeToUint(msg.Data[0 : 1])
            data := msg.Data[1 : ]
            if gproc.IsChild() {
                // ===============
                // 子进程
                // ===============
                // 任何与父进程的通信都会更新最后通信时间
                if msg.Pid == gproc.PPid() {
                    updateProcessChildUpdateTime()
                }
                switch act {
                    case gMSG_START:     onCommChildStart(msg.Pid, data)
                    case gMSG_RESTART:   onCommChildRestart(msg.Pid, data)
                    case gMSG_HEARTBEAT: onCommChildHeartbeat(msg.Pid, data)
                    case gMSG_SHUTDOWN:
                        onCommChildShutdown(msg.Pid, data)
                        return
                }
            } else {
                // ===============
                // 父进程
                // ===============
                // 任何进程消息都会自动更新最后通信时间记录
                if msg.Pid != gproc.Pid() {
                    updateProcessCommTime(msg.Pid)
                }
                if !procFirstTimeMap.Contains(msg.Pid) {
                    procFirstTimeMap.Set(msg.Pid, int(gtime.Millisecond()))
                }
                switch act {
                    case gMSG_START:     onCommMainStart(msg.Pid, data)
                    case gMSG_RESTART:   onCommMainRestart(msg.Pid, data)
                    case gMSG_NEW_FORK:  onCommMainNewFork(msg.Pid, data)
                    case gMSG_HEARTBEAT: onCommMainHeartbeat(msg.Pid, data)
                    case gMSG_REMOVE_PROC:
                        onCommMainRemoveProc(msg.Pid, data)
                        // 如果所有子进程都退出，那么主进程也主动退出
                        if procManager.Size() == 0 {
                            return
                        }
                    case gMSG_SHUTDOWN:
                        onCommMainShutdown(msg.Pid, data)
                        return
                }
            }
        }
    }
}

// 向进程发送操作消息
func sendProcessMsg(pid int, act int, data []byte) {
    gproc.Send(pid, formatMsgBuffer(act, data))
}

// 生成一条满足Web Server进程通信协议的消息
func formatMsgBuffer(act int, data []byte) []byte {
    return append(gbinary.EncodeUint8(uint8(act)), data...)
}

// 获取所有Web Server的文件描述符map
func getServerFdMap() map[string]listenerFdMap {
    sfm := make(map[string]listenerFdMap)
    serverMapping.RLockFunc(func(m map[string]interface{}) {
        for k, v := range m {
            sfm[k] = v.(*Server).getListenerFdMap()
        }
    })
    return sfm
}

// 二进制转换为FdMap
func bufferToServerFdMap(buffer []byte) map[string]listenerFdMap {
    sfm := make(map[string]listenerFdMap)
    if len(buffer) > 0 {
        j, _ := gjson.LoadContent(buffer, "json")
        for k, _ := range j.ToMap() {
            m := make(map[string]string)
            for k, v := range j.GetMap(k) {
                m[k] = gconv.String(v)
            }
            sfm[k] = m
        }
    }
    return sfm
}

// 关优雅闭进程所有端口的Web Server服务
// 注意，只是关闭Web Server服务，并不是退出进程
func shutdownWebServers() {
    serverMapping.RLockFunc(func(m map[string]interface{}) {
        for _, v := range m {
            for _, s := range v.(*Server).servers {
                s.shutdown()
            }
        }
    })
}

// 强制关闭进程所有端口的Web Server服务
// 注意，只是关闭Web Server服务，并不是退出进程
func closeWebServers() {
    serverMapping.RLockFunc(func(m map[string]interface{}) {
        for _, v := range m {
            for _, s := range v.(*Server).servers {
                s.close()
            }
        }
    })
}