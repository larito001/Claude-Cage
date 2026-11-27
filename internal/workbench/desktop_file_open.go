package workbench

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	pathpkg "path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/sylvinus/agent-vm/internal/host"
	"github.com/sylvinus/agent-vm/internal/paths"
)

type desktopFileOpen struct {
	ID      string `json:"id,omitempty"`
	Path    string `json:"path,omitempty"`
	Action  string `json:"action,omitempty"`
	Handled bool   `json:"handled,omitempty"`
}

// Resolve a guest path against directory grants, never against the host root.
// Both lexical traversal and a host-side symlink out of the project are denied.
func resolveProjectFilePath(guest string, projects []string) (string, error) {
	if !pathpkg.IsAbs(guest) || pathpkg.Clean(guest) != guest || strings.ContainsAny(guest, "\x00\\") {
		return "", errors.New("文件位置无效")
	}
	for _, project := range projects {
		if isClientMountPath(project) {
			continue
		}
		root := strings.TrimRight(guestPath(project), "/")
		if guest != root && !strings.HasPrefix(guest, root+"/") {
			continue
		}
		hostRoot, err := paths.Resolve(project)
		if err != nil || !paths.Equal(paths.Host(hostRoot), paths.Host(filepath.Clean(project))) {
			continue
		}
		target, err := paths.Resolve(filepath.Join(hostRoot, filepath.FromSlash(strings.TrimPrefix(guest, root))))
		if err != nil {
			return "", errors.New("文件已移动、删除或无法访问")
		}
		relative, err := filepath.Rel(hostRoot, target)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
			return "", errors.New("文件指向 SuperCage 项目目录之外，无法打开")
		}
		info, err := os.Stat(target)
		if err != nil || (!info.Mode().IsRegular() && !info.IsDir()) {
			return "", errors.New("请选择 SuperCage 项目中的普通文件或文件夹")
		}
		return target, nil
	}
	return "", errors.New("只能打开 SuperCage 项目目录中的文件和文件夹")
}

// Queue the same broker request as an application's file-manager action.
// The native client still claims the request and checks the grant at open time.
func (s *Server) desktopProjectFolder(w http.ResponseWriter, r *http.Request) {
	if !s.hasTrustedView(r) {
		http.Error(w, "请在 SuperCage 桌面应用中打开文件", http.StatusForbidden)
		return
	}
	var input struct {
		Desktop   string `json:"desktop"`
		Workspace string `json:"workspace"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	if input.Desktop != "default" && !validClaudeID(input.Desktop) {
		http.Error(w, "桌面实例标识无效", http.StatusBadRequest)
		return
	}
	if !s.fileTargetReady("desktop", "") {
		http.Error(w, "环境尚未就绪", http.StatusConflict)
		return
	}
	folder, err := s.desktopProjectFolderPath(input.Workspace)
	if err != nil {
		requestError(w, err)
		return
	}
	body, _ := json.Marshal(map[string]string{"path": folder})
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	_, err = s.guestFilesForDesktop(ctx, input.Desktop, "desktop", "POST", "/open", bytes.NewReader(body), "")
	if err != nil {
		requestError(w, err)
		return
	}
	jsonReply(w, map[string]bool{"ok": true})
}

func (s *Server) desktopProjectFolderPath(workspace string) (string, error) {
	if workspace == "" || isClientMountPath(workspace) || paths.Equal(paths.Host(workspace), paths.Host(s.environmentDir())) {
		return "", errors.New("请先通过打开项目添加该文件夹")
	}
	resolved, err := s.projectWorkspace(workspace)
	if err != nil {
		return "", err
	}
	if !paths.Equal(paths.Host(resolved), paths.Host(workspace)) {
		return "", errors.New("项目文件夹的位置已变更，请重新选择")
	}
	return guestPath(resolved), nil
}

func (s *Server) desktopFileOpenAction(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ID    string `json:"id"`
		Stage string `json:"stage"`
		Error string `json:"error"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	if !browserFileID.MatchString(input.ID) || (input.Stage != "claim" && input.Stage != "complete") {
		http.Error(w, "文件打开请求无效", 400)
		return
	}
	if !s.fileTargetReady("desktop", "") {
		http.Error(w, "环境尚未就绪", 409)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()
	desktopID := s.desktopFileOwner(input.ID)
	body, _ := json.Marshal(input)
	result, err := s.guestFilesForDesktop(ctx, desktopID, "desktop", "POST", "/open-"+input.Stage, bytes.NewReader(body), "")
	if err != nil {
		requestError(w, err)
		return
	}
	if input.Stage == "complete" || result["handled"] == true {
		jsonReply(w, result)
		return
	}
	guest, _ := result["path"].(string)
	action, _ := result["action"].(string)
	var target string
	if message, _ := result["error"].(string); message != "" {
		err = errors.New(message)
	} else if action != "open" && action != "reveal" {
		err = errors.New("文件打开操作无效")
	} else {
		var projects []string
		projects, err = s.projectPaths()
		if err == nil {
			target, err = resolveProjectFilePath(guest, projects)
		}
	}
	if err != nil {
		body, _ = json.Marshal(map[string]string{"id": input.ID, "error": err.Error()})
		cleanup, cancelCleanup := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancelCleanup()
		_, _ = s.guestFilesForDesktop(cleanup, desktopID, "desktop", "POST", "/open-complete", bytes.NewReader(body), "")
		requestError(w, err)
		return
	}
	jsonReply(w, desktopFileOpen{ID: input.ID, Path: target, Action: action})
}

// DesktopFileForConnection consumes one pending app request. The web bridge
// supplies only an opaque ID; the authenticated local controller owns its path.
func DesktopFileForConnection(configPath, connectionID, id string) (map[string]any, error) {
	if !browserFileID.MatchString(id) {
		return nil, errors.New("文件打开标识无效")
	}
	client, err := connectController(filepath.Dir(configPath))
	if err != nil {
		return nil, err
	}
	defer client.close()
	entry, _ := url.Parse(client.launch.URL)
	status, err := client.call("POST", "/api/session", map[string]string{"token": entry.Fragment}, nil)
	if err != nil || status != 200 {
		return nil, errors.New("无法连接文件操作会话")
	}
	connection, err := client.bindConnection(connectionID)
	if err != nil {
		return nil, err
	}
	var request desktopFileOpen
	err = desktopOpenCall(client, map[string]string{"id": id, "stage": "claim"}, &request)
	if err != nil {
		return nil, err
	}
	if request.Handled {
		return map[string]any{"handled": true}, nil
	}
	if connection.Mode == "remote" {
		err = errors.New("此文件位于远程环境，请先在下载列表中保存到本机")
	} else if request.ID != id || !filepath.IsAbs(request.Path) || (request.Action != "open" && request.Action != "reveal") {
		err = errors.New("文件打开响应无效")
	} else {
		err = openHostFile(request.Path, request.Action)
	}
	message := ""
	if err != nil {
		message = err.Error()
	}
	completeErr := desktopOpenCall(client, map[string]string{"id": id, "stage": "complete", "error": message}, nil)
	if err != nil {
		return nil, err
	}
	if completeErr != nil {
		return nil, completeErr
	}
	return map[string]any{"ok": true}, nil
}

func desktopOpenCall(client *controllerClient, input map[string]string, output any) error {
	body, _ := json.Marshal(input)
	request, _ := http.NewRequest("POST", client.origin+"/api/input/files/desktop/open", bytes.NewReader(body))
	request.Header.Set("Origin", client.origin)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.client.Do(request)
	if err != nil {
		return errors.New("文件操作连接已中断，请重新打开")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 65537))
	if err != nil || len(data) > 65536 {
		return errors.New("文件操作响应无效")
	}
	if response.StatusCode != http.StatusOK {
		message := strings.TrimSpace(string(data))
		if message == "" {
			message = "文件打开请求已失效，请重新操作"
		}
		return errors.New(message)
	}
	if output != nil && json.Unmarshal(data, output) != nil {
		return errors.New("文件操作响应无效")
	}
	return nil
}

var blockedHostOpenExtensions = map[string]bool{
	".action": true, ".app": true, ".appimage": true, ".applescript": true, ".appref-ms": true,
	".appx": true, ".bash": true, ".bat": true, ".bin": true, ".chm": true, ".cmd": true,
	".com": true, ".command": true, ".cpl": true, ".csh": true, ".deb": true, ".desktop": true,
	".diagcab": true, ".dll": true, ".dmg": true, ".exe": true, ".fileloc": true, ".fish": true,
	".gadget": true, ".hlp": true, ".hta": true, ".inetloc": true, ".inf": true, ".jar": true,
	".js": true, ".jse": true, ".ksh": true, ".library-ms": true, ".lnk": true, ".mpkg": true,
	".msc": true, ".msi": true, ".msix": true, ".msp": true, ".pif": true, ".pkg": true,
	".pl": true, ".ps1": true, ".psd1": true, ".psm1": true, ".py": true, ".pyw": true,
	".rb": true, ".reg": true, ".rpm": true, ".run": true, ".scf": true, ".scpt": true,
	".scptd": true, ".scr": true, ".searchconnector-ms": true, ".settingcontent-ms": true,
	".sh": true, ".terminal": true, ".tool": true, ".url": true, ".vbe": true, ".vbs": true,
	".webloc": true, ".workflow": true, ".wsf": true, ".wsh": true, ".zsh": true,
}

func isExecutablePathOrBundle(path string) bool {
	trimmed := strings.TrimRight(path, `/\ `)
	ext := strings.ToLower(filepath.Ext(trimmed))
	return blockedHostOpenExtensions[ext]
}

func hasExecutableFileHeader(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var magic [4]byte
	n, _ := io.ReadFull(f, magic[:])
	if n >= 2 && ((magic[0] == '#' && magic[1] == '!') || (magic[0] == 'M' && magic[1] == 'Z')) {
		return true
	}
	if n == 4 {
		switch string(magic[:]) {
		case "\x7fELF", "\xfe\xed\xfa\xce", "\xfe\xed\xfa\xcf", "\xce\xfa\xed\xfe", "\xcf\xfa\xed\xfe", "\xca\xfe\xba\xbe":
			return true
		}
	}
	return false
}

func hostFileCommand(platform, path, action string, directory bool) *exec.Cmd {
	if action == "open" && isExecutablePathOrBundle(path) {
		action = "reveal"
	}
	switch platform {
	case "darwin":
		if action == "reveal" {
			return exec.Command("/usr/bin/open", "-R", "--", path)
		}
		return exec.Command("/usr/bin/open", "--", path)
	case "windows":
		if action == "reveal" {
			return exec.Command("explorer.exe", "/select,"+path)
		}
		if directory {
			return exec.Command("explorer.exe", path)
		}
		return exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", path)
	default:
		if action == "reveal" {
			uri := (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
			return exec.Command("gdbus", "call", "--session", "--dest", "org.freedesktop.FileManager1", "--object-path", "/org/freedesktop/FileManager1", "--method", "org.freedesktop.FileManager1.ShowItems", "[\""+uri+"\"]", "")
		}
		return exec.Command("gio", "open", path)
	}
}

func openHostFile(path, action string) error {
	info, err := os.Stat(path)
	if err != nil || (!info.Mode().IsRegular() && !info.IsDir()) {
		return errors.New("文件已移动、删除或无法访问")
	}
	if action == "open" && (isExecutablePathOrBundle(path) || (info.Mode().IsRegular() && hasExecutableFileHeader(path))) {
		action = "reveal"
	}
	command := hostFileCommand(runtime.GOOS, path, action, info.IsDir())
	// Explorer hands the request to the desktop shell and can exit with status
	// 1 on success. Start it visibly and leave the window's lifetime to Windows.
	if runtime.GOOS == "windows" && (action == "reveal" || info.IsDir()) {
		if err := command.Start(); err != nil {
			return errors.New("无法启动 Windows 资源管理器")
		}
		return command.Process.Release()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, command.Path, command.Args[1:]...)
	host.HideCommandWindow(cmd)
	if err = cmd.Run(); err != nil {
		if runtime.GOOS != "linux" || action != "reveal" {
			return errors.New("无法打开本机文件或文件管理器，请检查默认应用和文件权限")
		}
		fallback := exec.CommandContext(ctx, "gio", "open", filepath.Dir(path))
		if fallback.Run() != nil {
			return errors.New("无法打开本机文件管理器，请检查桌面环境和文件权限")
		}
	}
	return nil
}
