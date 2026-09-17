//go:build linux

package headless

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// procRoot / bootIDPath 是 Linux 下的固定位置，抽成变量以便单元测试注入。
var (
	procRoot   = "/proc"
	bootIDPath = "/proc/sys/kernel/random/boot_id"
)

// errInvalidProcStat 表示 /proc/<pid>/stat 的内容不符合 proc(5) 的字段布局。
var errInvalidProcStat = errors.New("无法解析 /proc/<pid>/stat")

// processIdentity 读取 Linux 进程身份：Token = "<boot_id>:<starttime>"。
func processIdentity(pid int) (ProcessIdentity, error) {
	if pid <= 0 {
		return ProcessIdentity{}, fmt.Errorf("非法 PID: %d", pid)
	}
	fields, err := readProcStatFields(pid)
	if err != nil {
		return ProcessIdentity{}, err
	}
	start, err := starttimeFromStatFields(fields)
	if err != nil {
		return ProcessIdentity{}, err
	}
	boot, err := readBootID()
	if err != nil {
		return ProcessIdentity{}, err
	}
	return ProcessIdentity{PID: pid, Token: boot + ":" + start}, nil
}

// processGroupID 读取 PID 所在进程组的 PGID（/proc/<pid>/stat field 5）。
//
// 刻意不假设「PGID == PID」：core 目前用 setpgid 让 child 成为组长
// （internal/core/manager.go:142），但那属于 core 的实现细节；记录时按
// 实际读到的值记，将来 core 改变启动方式也不会记错。
func processGroupID(pid int) (int, error) {
	if pid <= 0 {
		return 0, fmt.Errorf("非法 PID: %d", pid)
	}
	fields, err := readProcStatFields(pid)
	if err != nil {
		return 0, err
	}
	if len(fields) <= 2 {
		return 0, errInvalidProcStat
	}
	pgid, err := strconv.Atoi(fields[2])
	if err != nil {
		return 0, fmt.Errorf("%w: pgrp=%q", errInvalidProcStat, fields[2])
	}
	return pgid, nil
}

// readProcStatFields 读取并解析 /proc/<pid>/stat；进程不存在时返回 ErrProcessGone。
func readProcStatFields(pid int) ([]string, error) {
	b, err := os.ReadFile(procRoot + "/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrProcessGone
		}
		return nil, err
	}
	return parseProcStatFields(string(b))
}

// parseProcStatFields 解析 /proc/<pid>/stat，返回 comm 之后的字段切片，
// 其中 index 0 就是 proc(5) 的 field 3（state）。
//
// **不能**对整行直接 strings.Fields 再取下标：第 2 个字段 comm 形如
// "(command name)"，允许包含空格，也可能包含 ')'。因此先定位**最后一个**
// ')'——comm 之后的字段全是数字，不会出现 ')'——再从它后面第一个字段开始切。
// 另需确认 ')' 之后紧跟的是分隔符，否则说明 ')' 出现在别的字段里。
func parseProcStatFields(line string) ([]string, error) {
	endComm := strings.LastIndex(line, ")")
	if endComm < 0 || endComm+1 >= len(line) {
		return nil, errInvalidProcStat
	}
	if line[endComm+1] != ' ' {
		return nil, errInvalidProcStat
	}
	fields := strings.Fields(line[endComm+1:])
	if len(fields) < 20 {
		return nil, errInvalidProcStat
	}
	return fields, nil
}

// starttimeFromStatFields 取 starttime：comm 之后的 index 19（= field 22）。
func starttimeFromStatFields(fields []string) (string, error) {
	if len(fields) <= 19 {
		return "", errInvalidProcStat
	}
	v := fields[19]
	if v == "" {
		return "", errInvalidProcStat
	}
	for i := 0; i < len(v); i++ {
		if v[i] < '0' || v[i] > '9' {
			return "", fmt.Errorf("%w: starttime=%q", errInvalidProcStat, v)
		}
	}
	return v, nil
}

// readBootID 读取本次启动的随机 ID。
//
// 读不到时**不降级**成「只用 starttime」：那正好会重新引入跨重启误判。
// 返回错误即代表身份不可验证，上层按 fail-closed 处理。
func readBootID() (string, error) {
	b, err := os.ReadFile(bootIDPath)
	if err != nil {
		return "", fmt.Errorf("读取 %s 失败（无法区分跨重启后的 PID/starttime 复用）: %w", bootIDPath, err)
	}
	id := strings.TrimSpace(string(b))
	if id == "" {
		return "", fmt.Errorf("%s 内容为空", bootIDPath)
	}
	return id, nil
}
