package bot

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/zinin/vpn-director/server/internal/chatstore"
	"github.com/zinin/vpn-director/server/internal/telegram"
	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

const (
	notificationMaxPerChat = 20
	notificationMaxAge     = 12 * time.Hour
	notificationLateAfter  = time.Minute
	notificationPollEvery  = 10 * time.Second
	notificationIPCTimeout = 2 * time.Second
	notificationWorkers    = 4
)

var errNotificationRevoked = errors.New("notification recipient is no longer active")

// notificationReceipt retains only delivery progress, never pending message text.
type notificationReceipt struct {
	eventID watchdapi.EventID
	at      time.Time
}

type notificationReceiver struct {
	once     sync.Once
	polling  sync.Mutex
	syncing  sync.Mutex
	mu       sync.Mutex
	sent     map[int64][]notificationReceipt
	inFlight map[int64]*notificationChatJob
	slots    chan struct{}
}

type notificationChatJob struct {
	chatID   int64
	receipts []notificationReceipt
	messages []watchdapi.Notification
}

type notificationWork struct {
	api watchdapi.NotificationAPI
	job *notificationChatJob
}

func withNotifications(api watchdapi.NotificationAPI) Option {
	return func(b *Bot) { b.notificationAPI = api }
}

func activeRecipients(store *chatstore.Store, auth *Auth) []watchdapi.Recipient {
	recipients := []watchdapi.Recipient{}
	if store == nil || auth == nil {
		return recipients
	}
	users, err := store.GetActiveUsers()
	if err != nil {
		return recipients
	}
	earliest := make(map[int64]time.Time, len(users))
	now := time.Now()
	for _, user := range users {
		if !auth.IsAuthorized(user.Username) || user.ChatID == 0 || user.FirstSeen.IsZero() || user.FirstSeen.After(now) {
			continue
		}
		if at, exists := earliest[user.ChatID]; !exists || user.FirstSeen.Before(at) {
			earliest[user.ChatID] = user.FirstSeen
		}
	}
	for chatID, firstSeen := range earliest {
		recipients = append(recipients, watchdapi.Recipient{ChatID: chatID, FirstSeen: firstSeen})
	}
	sort.Slice(recipients, func(i, j int) bool { return recipients[i].ChatID < recipients[j].ChatID })
	return recipients
}

func (b *Bot) notificationRecipients() []watchdapi.Recipient {
	b.mu.Lock()
	store, auth := b.chatStore, b.auth
	b.mu.Unlock()
	return activeRecipients(store, auth)
}

func (b *Bot) notificationSource() watchdapi.NotificationAPI {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.notificationAPI
}

func (b *Bot) syncRecipients(ctx context.Context) error {
	b.notifications.syncing.Lock()
	defer b.notifications.syncing.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	recipients := b.notificationRecipients()
	b.notifications.prune(recipients, time.Now())
	api := b.notificationSource()
	if api == nil {
		return nil
	}
	request, cancel := context.WithTimeout(ctx, notificationIPCTimeout)
	defer cancel()
	return api.SetRecipients(request, recipients)
}

func (r *notificationReceiver) prune(recipients []watchdapi.Recipient, now time.Time) {
	active := make(map[int64]time.Time, len(recipients))
	for _, recipient := range recipients {
		active[recipient.ChatID] = recipient.FirstSeen
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for chatID, receipts := range r.sent {
		firstSeen, allowed := active[chatID]
		kept := receipts[:0]
		for _, receipt := range receipts {
			if allowed && !receipt.at.Before(firstSeen) && now.Sub(receipt.at) <= notificationMaxAge {
				kept = append(kept, receipt)
			}
		}
		if len(kept) == 0 {
			delete(r.sent, chatID)
		} else {
			r.sent[chatID] = kept
		}
	}
}

func (r *notificationReceiver) remember(n watchdapi.Notification) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sent == nil {
		r.sent = make(map[int64][]notificationReceipt)
	}
	receipts := r.sent[n.ChatID]
	for _, receipt := range receipts {
		if receipt.eventID == n.EventID {
			return
		}
	}
	receipts = append(receipts, notificationReceipt{eventID: n.EventID, at: n.At})
	if len(receipts) > notificationMaxPerChat {
		receipts = receipts[len(receipts)-notificationMaxPerChat:]
	}
	r.sent[n.ChatID] = receipts
}

func (r *notificationReceiver) forget(chatID int64, eventID watchdapi.EventID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	receipts := r.sent[chatID]
	for i, receipt := range receipts {
		if receipt.eventID == eventID {
			receipts = append(receipts[:i], receipts[i+1:]...)
			break
		}
	}
	if len(receipts) == 0 {
		delete(r.sent, chatID)
	} else {
		r.sent[chatID] = receipts
	}
}

func (b *Bot) pollNotifications(ctx context.Context) error {
	api, jobs, err := b.readNotificationJobs(ctx)
	return errors.Join(err, b.deliverNotificationJobs(ctx, api, jobs))
}

func (b *Bot) readNotificationJobs(ctx context.Context) (watchdapi.NotificationAPI, map[int64]*notificationChatJob, error) {
	b.notifications.polling.Lock()
	defer b.notifications.polling.Unlock()
	syncErr := b.syncRecipients(ctx)
	api := b.notificationSource()
	if api == nil || ctx.Err() != nil {
		return api, nil, errors.Join(syncErr, ctx.Err())
	}
	recipients := b.notificationRecipients()
	allowed := make(map[int64]time.Time, len(recipients))
	for _, recipient := range recipients {
		allowed[recipient.ChatID] = recipient.FirstSeen
	}
	jobs := make(map[int64]*notificationChatJob, len(recipients))
	b.notifications.mu.Lock()
	// Exclude busy chats for the whole read, including jobs that finish mid-page.
	busy := make(map[int64]bool, len(b.notifications.inFlight))
	for chatID := range b.notifications.inFlight {
		busy[chatID] = true
	}
	for chatID, receipts := range b.notifications.sent {
		if !busy[chatID] {
			jobs[chatID] = &notificationChatJob{chatID: chatID, receipts: append([]notificationReceipt{}, receipts...)}
		}
	}
	b.notifications.mu.Unlock()

	cursor := ""
	seenCursors := make(map[string]bool)
	seenEvents := make(map[notificationDeliveryKey]bool)
	var pageErr error
	// A stable queue has at most 20 events per authorized chat, even with byte-limited pages.
	for pages := 0; pages <= len(recipients)*notificationMaxPerChat; pages++ {
		request, cancel := context.WithTimeout(ctx, notificationIPCTimeout)
		page, err := api.Pending(request, cursor)
		cancel()
		if err != nil {
			pageErr = err
			break
		}
		for _, n := range page.Messages {
			firstSeen, authorized := allowed[n.ChatID]
			key := notificationDeliveryKey{chatID: n.ChatID, eventID: n.EventID}
			if busy[n.ChatID] || !authorized || n.At.Before(firstSeen) || time.Since(n.At) > notificationMaxAge || seenEvents[key] {
				continue
			}
			job := jobs[n.ChatID]
			if job == nil {
				job = &notificationChatJob{chatID: n.ChatID}
				jobs[n.ChatID] = job
			}
			if len(job.messages) < notificationMaxPerChat {
				job.messages = append(job.messages, n)
				seenEvents[key] = true
			}
		}
		if page.NextCursor == "" {
			break
		}
		if page.NextCursor == cursor || seenCursors[page.NextCursor] {
			pageErr = errors.New("notification cursor did not advance")
			break
		}
		seenCursors[page.NextCursor] = true
		cursor = page.NextCursor
	}
	b.notifications.mu.Lock()
	if b.notifications.inFlight == nil {
		b.notifications.inFlight = make(map[int64]*notificationChatJob)
	}
	for chatID, job := range jobs {
		b.notifications.inFlight[chatID] = job
	}
	b.notifications.mu.Unlock()
	return api, jobs, errors.Join(syncErr, pageErr, ctx.Err())
}

func (r *notificationReceiver) finish(job *notificationChatJob) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.inFlight[job.chatID] == job {
		delete(r.inFlight, job.chatID)
	}
}

type notificationDeliveryKey struct {
	chatID  int64
	eventID watchdapi.EventID
}

func (b *Bot) deliverNotificationJobs(ctx context.Context, api watchdapi.NotificationAPI, jobs map[int64]*notificationChatJob) error {
	if len(jobs) == 0 {
		return nil
	}
	defer func() {
		for _, job := range jobs {
			b.notifications.finish(job)
		}
	}()
	chats := make([]int64, 0, len(jobs))
	for chatID := range jobs {
		chats = append(chats, chatID)
	}
	sort.Slice(chats, func(i, j int) bool { return chats[i] < chats[j] })
	work := make(chan *notificationChatJob)
	results := make(chan error, len(jobs))
	var workers sync.WaitGroup
	for i := 0; i < notificationWorkers && i < len(jobs); i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for job := range work {
				results <- b.deliverNotificationChat(ctx, api, job)
			}
		}()
	}

dispatch:
	for _, chatID := range chats {
		select {
		case <-ctx.Done():
			break dispatch
		case work <- jobs[chatID]:
		}
	}
	close(work)
	workers.Wait()
	close(results)
	result := ctx.Err()
	for err := range results {
		if result == nil && err != nil {
			result = err
		}
	}
	return result
}

func (b *Bot) notificationAllowed(chatID int64, at time.Time) bool {
	if time.Since(at) > notificationMaxAge {
		return false
	}
	for _, recipient := range b.notificationRecipients() {
		if recipient.ChatID == chatID && !at.Before(recipient.FirstSeen) {
			return true
		}
	}
	return false
}

func (b *Bot) notificationSender() telegram.MessageSender {
	b.mu.Lock()
	sender, pathLive := b.sender, b.pathLive
	b.mu.Unlock()
	if sender == nil || pathLive != nil && !pathLive() {
		return nil
	}
	return sender
}

func (b *Bot) sendNotification(ctx context.Context, n watchdapi.Notification) error {
	text := notificationText(n, time.Now())
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !b.notificationAllowed(n.ChatID, n.At) {
			return errNotificationRevoked
		}
		sender := b.notificationSender()
		if sender == nil {
			return errNoPath
		}
		block, rest := notificationBlock(text)
		if err := sender.SendPlain(n.ChatID, block); err != nil {
			return err
		}
		if rest == "" {
			return nil
		}
		text = rest
	}
}

func (b *Bot) ackNotification(ctx context.Context, api watchdapi.NotificationAPI, chatID int64, eventID watchdapi.EventID) error {
	request, cancel := context.WithTimeout(ctx, notificationIPCTimeout)
	defer cancel()
	if err := api.Ack(request, chatID, eventID); err != nil {
		return err
	}
	b.notifications.forget(chatID, eventID)
	return nil
}

func (b *Bot) deliverNotificationChat(ctx context.Context, api watchdapi.NotificationAPI, job *notificationChatJob) error {
	defer b.notifications.finish(job)
	b.notifications.mu.Lock()
	if b.notifications.slots == nil {
		b.notifications.slots = make(chan struct{}, notificationWorkers)
	}
	slots := b.notifications.slots
	b.notifications.mu.Unlock()
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	case <-ctx.Done():
		return ctx.Err()
	}
	closed := make(map[watchdapi.EventID]bool, len(job.receipts))
	var result error
	for _, receipt := range job.receipts {
		if !b.notificationAllowed(job.chatID, receipt.at) {
			b.notifications.forget(job.chatID, receipt.eventID)
			continue
		}
		closed[receipt.eventID] = true
		if err := b.ackNotification(ctx, api, job.chatID, receipt.eventID); result == nil && err != nil {
			result = err
		}
	}
	for _, n := range job.messages {
		if err := ctx.Err(); err != nil {
			return err
		}
		if closed[n.EventID] {
			continue
		}
		if !b.notificationAllowed(n.ChatID, n.At) {
			continue
		}
		err := b.sendNotification(ctx, n)
		if errors.Is(err, errNotificationRevoked) {
			return errors.Join(result, b.syncRecipients(ctx))
		}
		if err != nil && retryable(err) {
			return errors.Join(result, err)
		}
		var apiErr *tgbotapi.Error
		if errors.As(err, &apiErr) && apiErr.Code == http.StatusForbidden {
			b.mu.Lock()
			store := b.chatStore
			b.mu.Unlock()
			var inactiveErr error
			if store != nil {
				inactiveErr = store.SetInactiveChat(n.ChatID)
			}
			return errors.Join(result, inactiveErr, b.syncRecipients(ctx), b.ackNotification(ctx, api, n.ChatID, n.EventID))
		}
		if b.notificationAllowed(n.ChatID, n.At) {
			b.notifications.remember(n)
		}
		if err := b.ackNotification(ctx, api, n.ChatID, n.EventID); result == nil && err != nil {
			result = err
		}
	}
	return result
}

func (b *Bot) receiveNotifications(ctx context.Context) {
	b.notifications.once.Do(func() {
		ticker := time.NewTicker(notificationPollEvery)
		defer ticker.Stop()
		work := make(chan notificationWork)
		results := make(chan error, notificationWorkers)
		var workers sync.WaitGroup
		for i := 0; i < notificationWorkers; i++ {
			workers.Add(1)
			go func() {
				defer workers.Done()
				for task := range work {
					err := b.deliverNotificationChat(ctx, task.api, task.job)
					select {
					case results <- err:
					case <-ctx.Done():
					}
				}
			}()
		}
		var queue []notificationWork
		defer func() {
			for _, task := range queue {
				b.notifications.finish(task.job)
			}
			close(work)
			workers.Wait()
		}()
		poll := func() {
			api, jobs, err := b.readNotificationJobs(ctx)
			if err != nil && ctx.Err() == nil {
				slog.Warn("Watch notification delivery will retry")
			}
			chats := make([]int64, 0, len(jobs))
			for chatID := range jobs {
				chats = append(chats, chatID)
			}
			sort.Slice(chats, func(i, j int) bool { return chats[i] < chats[j] })
			for _, chatID := range chats {
				queue = append(queue, notificationWork{api: api, job: jobs[chatID]})
			}
		}
		poll()
		for {
			if ctx.Err() != nil {
				return
			}
			var dispatch chan<- notificationWork
			var next notificationWork
			if len(queue) > 0 {
				dispatch, next = work, queue[0]
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				poll()
			case dispatch <- next:
				queue[0] = notificationWork{}
				queue = queue[1:]
			case err := <-results:
				if err != nil && ctx.Err() == nil {
					slog.Warn("Watch notification delivery will retry")
				}
			}
		}
	})
}

// retryable excludes permanent Telegram refusals, but retains transport errors.
func retryable(err error) bool {
	var apiErr *tgbotapi.Error
	if errors.As(err, &apiErr) {
		return apiErr.Code == http.StatusTooManyRequests || apiErr.Code >= http.StatusInternalServerError
	}
	return true
}
