package worker

var ExportHandleMessage = (*Worker).handleMessage

// WaitForTasks waits until every task w's work subscription has dispatched
// so far has finished. Call it after a synchronous natsbustest.Fake.Deliver
// returns, so specs can assert on the task's effects.
func WaitForTasks(w *Worker) {
	w.mu.Lock()
	run := w.run
	w.mu.Unlock()
	if run != nil {
		run.tasks.Wait()
	}
}
