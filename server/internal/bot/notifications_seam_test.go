package bot

import (
	"context"
	"errors"
	"sort"
	"sync"

	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

// pollNotifications and deliverNotificationJobs are a synchronous test seam
// over the production primitives readNotificationJobs and
// deliverNotificationChat: one poll, then its jobs delivered before it returns.
// Production dispatch is receiveNotifications, with its persistent queue and
// workers.
func (b *Bot) pollNotifications(ctx context.Context) error {
	api, jobs, err := b.readNotificationJobs(ctx)
	return errors.Join(err, b.deliverNotificationJobs(ctx, api, jobs))
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
