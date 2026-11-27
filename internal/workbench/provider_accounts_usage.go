package workbench

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"al.essio.dev/pkg/shellescape"
	"github.com/klauspost/compress/gzip"
	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/diagnostics"
)

//go:embed provider_usage.py
var providerUsageHelper string

var providerUsageEncoded = func() string {
	var data bytes.Buffer
	w, _ := gzip.NewWriterLevel(&data, gzip.BestCompression)
	_, _ = w.Write([]byte(providerUsageHelper))
	_ = w.Close()
	return cliTransport85(data.Bytes())
}()

func providerUsageCommand(provider, mode string, args ...string) string {
	code := fmt.Sprintf("import base64,gzip;exec(compile(gzip.decompress(base64.b85decode('%s')),'<provider-usage>','exec'))", providerUsageEncoded)
	argv := append([]string{"python3", "-I", "-c", code, provider, mode}, args...)
	return shellescape.QuoteCommand(argv)
}

type providerGuestQuota struct {
	providerQuota
	Identity string `json:"identity"`
}

func cloneClaudeWindow(w *claudeWindow) *claudeWindow {
	if w == nil {
		return nil
	}
	cp := *w
	return &cp
}

func (a *providerAccount) mergeQuota(q *providerQuota, now time.Time) bool {
	return mergeProviderQuotaPtr(&a.Quota, q, now)
}

func mergeProviderQuotaPtr(dst **providerQuota, q *providerQuota, now time.Time) bool {
	if q == nil || q.ObservedAt <= 0 || q.ObservedAt > now.Unix()+60 {
		return false
	}
	observed := min(q.ObservedAt, now.Unix())
	next := providerQuota{}
	if *dst != nil {
		next = **dst
		if len((*dst).Groups) > 0 {
			next.Groups = make([]providerQuotaGroup, len((*dst).Groups))
			for i, g := range (*dst).Groups {
				next.Groups[i] = providerQuotaGroup{
					ID:       g.ID,
					Label:    g.Label,
					FiveHour: cloneClaudeWindow(g.FiveHour),
					SevenDay: cloneClaudeWindow(g.SevenDay),
				}
			}
		}
	}
	changed := false
	mergeWin := func(old, reading *claudeWindow) *claudeWindow {
		if !validClaudeWindow(reading) {
			return old
		}
		window := *reading
		if window.ObservedAt == 0 {
			window.ObservedAt = observed
		}
		if window.ObservedAt > now.Unix() {
			window.ObservedAt = now.Unix()
		}
		if window.ObservedAt < 0 || window.ObservedAt > observed || (old != nil && old.ObservedAt > window.ObservedAt) {
			return old
		}
		if old != nil && window == *old {
			return old
		}
		changed = true
		return &window
	}
	next.FiveHour = mergeWin(next.FiveHour, q.FiveHour)
	next.SevenDay = mergeWin(next.SevenDay, q.SevenDay)

	if len(q.Groups) > 0 {
		mergedGroups := make([]providerQuotaGroup, 0, len(q.Groups))
		for _, g := range q.Groups {
			if g.ID == "" || g.Label == "" || len(g.ID) > 32 || len(g.Label) > 48 {
				continue
			}
			var prevGroup providerQuotaGroup
			for _, oldG := range next.Groups {
				if oldG.ID == g.ID {
					prevGroup = oldG
					break
				}
			}
			fh := mergeWin(prevGroup.FiveHour, g.FiveHour)
			sd := mergeWin(prevGroup.SevenDay, g.SevenDay)
			if fh == nil && sd == nil {
				continue
			}
			if prevGroup.Label != g.Label {
				changed = true
			}
			mergedGroups = append(mergedGroups, providerQuotaGroup{
				ID:       g.ID,
				Label:    g.Label,
				FiveHour: fh,
				SevenDay: sd,
			})
		}
		if len(mergedGroups) > 0 {
			next.Groups = mergedGroups
		}
	}

	if q.SingleWindow == "five_hour" || q.SingleWindow == "seven_day" {
		if next.SingleWindow != q.SingleWindow {
			next.SingleWindow = q.SingleWindow
			changed = true
		}
	} else if next.SingleWindow != "" && next.FiveHour != nil && next.SevenDay != nil {
		next.SingleWindow = ""
		changed = true
	}

	if next.FiveHour == nil && next.SevenDay == nil && len(next.Groups) == 0 {
		return false
	}
	if !changed && *dst != nil {
		return false
	}
	next.ObservedAt = max(next.ObservedAt, observed)
	*dst = &next
	return true
}

func (st *providerAccountState) observeDefaultSnapshot(snap providerGuestSnapshot, now time.Time) {
	st.defaultReading = snap
	subj := snap.resolvedSubject()
	if subj == "" {
		return
	}
	if st.defaultQuotaSubject != subj {
		st.defaultQuotaSubject = subj
		st.defaultQuota = nil
		st.defaultUsageStatus = ""
		st.defaultUsageCheckedAt = 0
	}
	if snap.Quota != nil {
		mergeProviderQuotaPtr(&st.defaultQuota, snap.Quota, now)
	}
}

func (a providerAccount) headroom(now time.Time) (float64, bool) {
	if a.Quota == nil {
		return -1, true
	}
	least := -1.0
	for _, w := range []*claudeWindow{a.Quota.FiveHour, a.Quota.SevenDay} {
		if !validClaudeWindow(w) || (w.ResetsAt > 0 && w.ResetsAt <= now.Unix()) {
			continue
		}
		fresh := now.Sub(time.Unix(w.ObservedAt, 0)) <= claudeQuotaFresh
		if w.UsedPercentage >= 100 && (w.ResetsAt > now.Unix() || fresh) {
			return 0, false
		}
		if fresh {
			remaining := 100 - w.UsedPercentage
			if least < 0 || remaining < least {
				least = remaining
			}
		}
	}
	return least, true
}

func (s *Server) readProviderGuestUsage(ctx context.Context, provider, guestID, subject string) (providerGuestSnapshot, error) {
	var snap providerGuestSnapshot
	if !validClaudeGuestID(guestID) || !validClaudeSubject(subject) {
		return snap, errors.New("账号身份标识无效")
	}
	cmd := "exec " + providerUsageCommand(provider, "usage", guestID, subject)
	if guestID == "default" {
		cmd = `set -o pipefail; cd "$HOME" && env TERM=xterm-256color COLORTERM=truecolor CAGE_WORKBENCH=1 /bin/bash -lic ` +
			shellescape.Quote(`printf '\nCAGE_PROVIDER_USAGE '; `+cmd) +
			` | sed -n 's/^CAGE_PROVIDER_USAGE //p'`
	}
	a, err := s.boundProviderGuestAccount(provider, guestID, subject)
	if err != nil {
		return snap, err
	}
	if a != nil {
		if cmd, err = s.routeSelected(ctx, *a, cmd); err != nil {
			return snap, err
		}
	}
	data, stderr, err := s.guestControlWithStderr(ctx, cmd)
	if err != nil {
		probeErr := classifyAccountProbeError(err, stderr)
		diagnostics.Log("warn", "provider-accounts", fmt.Sprintf("provider=%s guest=%s usage probe failed: exit=%d err=%v stderr=%q", provider, guestID, probeErr.exitCode, err, sanitizeAccountNetworkStderr(stderr)))
		return snap, probeErr
	}
	if err := json.Unmarshal(data, &snap); err != nil {
		return snap, errors.New("无法解析账号额度读数，请重试")
	}
	return snap, nil
}

func (s *Server) readProviderGuestQuotas(ctx context.Context, provider string, ids []string) (map[string]providerGuestQuota, error) {
	for _, id := range ids {
		if !validClaudeGuestID(id) {
			return nil, errors.New("账号目录标识无效")
		}
	}
	readings := make(map[string]providerGuestQuota)
	for chunk := range slices.Chunk(ids, 96) {
		cmd := "exec " + providerUsageCommand(provider, "quotas", chunk...)
		data, err := s.guest(ctx, cmd)
		if err != nil {
			return readings, err
		}
		if err := json.Unmarshal(data, &readings); err != nil {
			return readings, err
		}
	}
	return readings, nil
}

func (s *Server) startProviderPanelUsage(provider string, list providerAccountList) {
	if s.usageAvailability() != nil {
		return
	}
	epoch, unlocked := s.account.access()
	if !unlocked {
		return
	}
	st := s.providerAccounts.forProvider(provider)
	st.mu.Lock()
	st.panelUntil = time.Now().Add(claudePanelLease)
	if st.panelRunning {
		st.mu.Unlock()
		return
	}
	var targets []claudeUsageTarget
	defSub := st.defaultReading.resolvedSubject()
	if st.defaultError == nil && st.defaultReading.LoggedIn && defSub != "" && list.bySubject(defSub) == nil && !slices.Contains(list.DismissedDefaults, defSub) {
		var checkedAt int64
		if st.defaultQuotaSubject == defSub {
			checkedAt = st.defaultUsageCheckedAt
		}
		if claudeUsageDue(checkedAt) {
			targets = append(targets, claudeUsageTarget{guest: "default", subject: defSub})
		}
	}
	for _, a := range list.Accounts {
		sub := a.subject()
		if a.Status != "ready" || a.CodeUnlinked || sub == "" || !claudeUsageDue(a.UsageCheckedAt) {
			continue
		}
		targets = append(targets, claudeUsageTarget{id: a.ID, guest: a.guestID(), subject: sub})
	}
	if len(targets) == 0 {
		st.mu.Unlock()
		return
	}
	st.panelRunning = true
	st.mu.Unlock()

	ctx, finish, err := s.beginActivity(epoch)
	if err != nil {
		st.mu.Lock()
		st.panelRunning = false
		st.mu.Unlock()
		return
	}
	go func() {
		defer finish()
		defer func() {
			st.mu.Lock()
			st.panelRunning = false
			st.mu.Unlock()
		}()
		for i := 0; i < len(targets); {
			st.mu.Lock()
			visible := time.Now().Before(st.panelUntil)
			st.mu.Unlock()
			if !visible || ctx.Err() != nil || s.usageAvailability() != nil {
				return
			}
			if err := s.checkProviderUsage(ctx, provider, targets[i], true); !errors.Is(err, errClaudeUsageBusy) {
				i++
				continue
			}
			st.mu.Lock()
			var done <-chan struct{}
			for _, call := range st.usageActive {
				done = call.done
				break
			}
			st.mu.Unlock()
			if done != nil {
				select {
				case <-ctx.Done():
					return
				case <-done:
				}
			}
		}
	}()
}

func (s *Server) refreshProviderUsage(ctx context.Context, provider string, input providerAccountInput) error {
	st := s.providerAccounts.forProvider(provider)
	if input.Action == "refresh_default" {
		snap, probeErr := s.probeProviderGuest(ctx, provider, "default")
		now := time.Now()
		st.mu.Lock()
		st.defaultChecked = now
		st.defaultError = probeErr
		if probeErr == nil {
			st.observeDefaultSnapshot(snap, now)
		}
		st.mu.Unlock()
		if probeErr != nil {
			return probeErr
		}
		sub := snap.resolvedSubject()
		if input.Identity != "" && sub != input.Identity {
			return errors.New("默认登录身份已经变化，请刷新账号面板")
		}
		if sub == "" || s.usageAvailability() != nil {
			return s.usageAvailability()
		}
		return s.checkProviderUsage(ctx, provider, claudeUsageTarget{guest: "default", subject: sub}, false)
	}

	if !validClaudeID(input.ID) {
		return errors.New("账号标识无效")
	}
	st.mu.Lock()
	list, err := s.loadProviderAccounts(provider)
	var guestID string
	if err == nil {
		if a := list.find(input.ID); a != nil {
			guestID = a.guestID()
		} else {
			err = errors.New("未找到该账号")
		}
	}
	st.mu.Unlock()
	if err != nil {
		return err
	}

	snap, probeErr := s.probeProviderGuest(ctx, provider, guestID)
	if probeErr != nil {
		return probeErr
	}
	now := time.Now()
	var target claudeUsageTarget
	st.mu.Lock()
	list, err = s.loadProviderAccounts(provider)
	if err == nil {
		if a := list.find(input.ID); a != nil {
			a.CheckedAt = now.Unix()
			a.AuthState = snap.AuthState
			if snap.LoggedIn && snap.resolvedSubject() != "" {
				if a.Email == "" || a.subject() == snap.resolvedSubject() {
					a.Email = strings.ToLower(strings.TrimSpace(snap.Email))
					a.Organization = strings.TrimSpace(snap.Organization)
					if snap.Plan != "" {
						a.Plan = snap.Plan
					}
					if a.Label == "" {
						a.Label = a.Email
					}
					a.Status = "ready"
					a.CodeUnlinked = false
					a.mergeQuota(snap.Quota, now)
					target = claudeUsageTarget{id: a.ID, guest: a.guestID(), subject: a.subject()}
				} else {
					a.Status = "changed"
				}
			} else {
				a.Status = "signed_out"
			}
			err = s.saveProviderAccounts(provider, list)
		} else {
			err = errors.New("未找到该账号")
		}
	}
	st.mu.Unlock()
	if err != nil {
		return err
	}
	if target.subject == "" {
		return nil
	}
	if availErr := s.usageAvailability(); availErr != nil {
		return availErr
	}
	return s.checkProviderUsage(ctx, provider, target, false)
}

func (s *Server) checkProviderUsage(parent context.Context, provider string, target claudeUsageTarget, automatic bool) (err error) {
	if err = s.usageAvailability(); err != nil {
		return err
	}
	epoch, unlocked := s.account.access()
	if !unlocked {
		return errClaudeAccessLost
	}
	ctx, cancel := context.WithTimeout(parent, 22*time.Second)
	stop := context.AfterFunc(epoch, cancel)
	defer func() { stop(); cancel() }()

	st := s.providerAccounts.forProvider(provider)
	st.mu.Lock()
	if call := st.usageActive[target.subject]; call != nil {
		st.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-call.done:
			return call.err
		}
	}
	list, loadErr := s.loadProviderAccounts(provider)
	if loadErr != nil {
		st.mu.Unlock()
		return loadErr
	}
	isUnregisteredDefault := target.id == "" && target.guest == "default"
	var checkedAt int64
	if isUnregisteredDefault {
		if st.defaultReading.resolvedSubject() != target.subject || list.bySubject(target.subject) != nil {
			st.mu.Unlock()
			return errors.New("账号来源或身份已变化，请刷新面板")
		}
		if st.defaultQuotaSubject == target.subject {
			checkedAt = st.defaultUsageCheckedAt
		}
	} else {
		owner := s.providerUsageOwner(list, target)
		if owner == nil {
			st.mu.Unlock()
			return errors.New("账号来源或身份已变化，请刷新面板")
		}
		checkedAt = owner.UsageCheckedAt
	}
	if automatic && !claudeUsageDue(checkedAt) {
		st.mu.Unlock()
		return nil
	}
	if len(st.usageActive) >= 2 {
		st.mu.Unlock()
		return errClaudeUsageBusy
	}
	if st.usageActive == nil {
		st.usageActive = make(map[string]*claudeUsageCall)
	}
	call := &claudeUsageCall{done: make(chan struct{})}
	st.usageActive[target.subject] = call
	probe := st.usageProbe
	st.mu.Unlock()

	defer func() {
		st.mu.Lock()
		delete(st.usageActive, target.subject)
		call.err = err
		close(call.done)
		st.mu.Unlock()
	}()

	if probe == nil {
		probe = s.readProviderGuestUsage
	}
	reading, probeErr := probe(ctx, provider, target.guest, target.subject)
	if epoch.Err() != nil {
		return errClaudeAccessLost
	}
	if parent.Err() != nil {
		return parent.Err()
	}
	if err = s.usageAvailability(); err != nil {
		return err
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	if epoch.Err() != nil {
		return errClaudeAccessLost
	}
	if parent.Err() != nil {
		return parent.Err()
	}
	list, err = s.loadProviderAccounts(provider)
	if err != nil {
		return err
	}

	now := time.Now()
	status := reading.UsageStatus
	switch {
	case probeErr != nil || status == "error":
		status = "error"
	case status == "identity_changed" || reading.resolvedSubject() != target.subject:
		status = "identity_changed"
	case status == "reported" || status == "partial" || status == "cached":
	case status == "unavailable" || status == "not_applicable":
	default:
		status = "error"
	}

	if isUnregisteredDefault {
		if st.defaultReading.resolvedSubject() != target.subject {
			return errors.New("账号来源或身份已变化，未记录此次额度")
		}
		if status == "identity_changed" {
			st.defaultChecked = time.Time{}
			st.defaultQuota = nil
			st.defaultQuotaSubject = ""
			st.defaultUsageStatus = ""
			st.defaultUsageCheckedAt = 0
			return nil
		}
		if st.defaultQuotaSubject != target.subject {
			st.defaultQuotaSubject = target.subject
			st.defaultQuota = nil
		}
		if status == "reported" || status == "partial" || status == "cached" {
			mergeProviderQuotaPtr(&st.defaultQuota, reading.Quota, now)
		}
		if status != "error" && reading.Plan != "" {
			st.defaultReading.Plan = reading.Plan
		}
		st.defaultUsageStatus = status
		st.defaultUsageCheckedAt = now.Unix()
		return nil
	}

	owner := s.providerUsageOwner(list, target)
	if owner == nil {
		return errors.New("账号来源或身份已变化，未记录此次额度")
	}
	if status == "identity_changed" {
		owner.Status = "changed"
	} else {
		if status == "reported" || status == "partial" || status == "cached" {
			owner.mergeQuota(reading.Quota, now)
		}
		if status != "error" {
			owner.Status = "ready"
			if reading.Plan != "" {
				owner.Plan = reading.Plan
			}
		}
	}
	owner.UsageStatus = status
	owner.UsageCheckedAt = now.Unix()
	return s.saveProviderAccounts(provider, list)
}

func (s *Server) providerUsageOwner(list providerAccountList, target claudeUsageTarget) *providerAccount {
	if a := list.bySubject(target.subject); a != nil {
		if a.guestID() == target.guest && (target.id == "" || target.id == a.ID) {
			return a
		}
	}
	return nil
}

func (s *Server) allocateProviderAccountLocked(provider string, st *providerAccountState, list providerAccountList, workspace string) (claudeSelection, error) {
	spec, _ := cage.LookupProvider(provider)
	if len(list.Accounts) == 0 {
		return claudeSelection{GuestID: "default"}, nil
	}

	var probeIDs []string
	for _, a := range list.Accounts {
		if a.Enabled && a.Status == "ready" && !a.CodeUnlinked && a.subject() != "" {
			probeIDs = append(probeIDs, a.guestID())
		}
	}
	qProbe := st.quotasProbe
	if qProbe == nil && s.vmRunning() {
		qProbe = s.readProviderGuestQuotas
	}
	if qProbe != nil && len(probeIDs) > 0 {
		if epoch, ok := s.account.access(); ok {
			st.mu.Unlock()
			ctx, cancel := context.WithTimeout(epoch, 3*time.Second)
			readings, _ := qProbe(ctx, provider, probeIDs)
			cancel()
			st.mu.Lock()
			if reloaded, err := s.loadProviderAccounts(provider); err == nil {
				list = reloaded
				now := time.Now()
				updated := false
				for i := range list.Accounts {
					a := &list.Accounts[i]
					if q, ok := readings[a.guestID()]; ok && a.subject() != "" && q.Identity == a.subject() {
						if a.mergeQuota(&q.providerQuota, now) {
							updated = true
						}
					}
				}
				if updated {
					_ = s.saveProviderAccounts(provider, list)
				}
			}
		}
	}

	now := time.Now()
	active := make(map[string]int)
	logins := make(map[string]bool)
	for _, t := range s.terminals.snapshot() {
		if t["closed"] == true || t["provider"] != provider {
			continue
		}
		aid, _ := t["provider_account_id"].(string)
		if aid == "" {
			continue
		}
		if isProviderLoginKind(fmt.Sprint(t["kind"])) {
			logins[aid] = true
		} else {
			active[aid]++
		}
	}

	var chosen *providerAccount
	if list.Scheduling.Mode == "sequential" {
		for i := range list.Accounts {
			a := &list.Accounts[i]
			if _, ok := a.headroom(now); a.Enabled && a.Status == "ready" && !a.CodeUnlinked && !logins[a.ID] && a.subject() != "" && ok {
				chosen = a
				break
			}
		}
	} else {
		start := slices.IndexFunc(list.Accounts, func(a providerAccount) bool { return a.ID == st.lastPick }) + 1
		var bestActive int
		var bestRoom float64
		for offset := range list.Accounts {
			a := &list.Accounts[(start+offset)%len(list.Accounts)]
			if !a.Enabled || a.Status != "ready" || a.CodeUnlinked || logins[a.ID] || a.subject() == "" {
				continue
			}
			room, ok := a.headroom(now)
			if ok && (chosen == nil || active[a.ID] < bestActive || (active[a.ID] == bestActive && room > bestRoom)) {
				chosen, bestActive, bestRoom = a, active[a.ID], room
			}
		}
		if workspace != "" {
			if preferredID := st.recent[workspace]; preferredID != "" {
				if pref := list.find(preferredID); pref != nil && pref.Enabled && pref.Status == "ready" && !pref.CodeUnlinked && !logins[pref.ID] && pref.subject() != "" && active[pref.ID] == bestActive {
					if room, ok := pref.headroom(now); ok && (room == bestRoom || (room >= 0 && bestRoom-room <= min(5, bestRoom*0.1))) {
						chosen = pref
					}
				}
			}
		}
	}

	if chosen == nil {
		return claudeSelection{}, fmt.Errorf("当前没有可分配的 %s 账号，请在账号管理中检查登录、启用状态和额度重置时间", spec.Name)
	}
	st.lastPick = chosen.ID
	if workspace != "" {
		st.recent[workspace] = chosen.ID
	}
	return claudeSelection{ID: chosen.ID, GuestID: chosen.guestID(), Subject: chosen.subject()}, nil
}
