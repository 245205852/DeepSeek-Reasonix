import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { TaskMonitorPanel } from "./TaskMonitorPanel";

const mockListTasks = vi.fn();
const mockListTaskEvents = vi.fn();
const mockRequeueTask = vi.fn();

vi.mock("../lib/bridge", () => ({
  app: {
    ListTasks: (...args: unknown[]) => mockListTasks(...args),
    ListTaskEvents: (...args: unknown[]) => mockListTaskEvents(...args),
    StopTask: vi.fn(),
    CancelTask: vi.fn(),
    RequeueTask: (...args: unknown[]) => mockRequeueTask(...args),
    OpenTaskSession: vi.fn(),
    GetTask: vi.fn().mockResolvedValue(null),
  },
}));

function snap(overrides: Record<string, unknown> = {}) {
  return {
    schema_version: 1,
    task_id: "task-0001",
    session_id: "sess-1",
    state: "running",
    runtime_state: "alive",
    version: 1,
    created_at: "2025-01-01T00:00:00Z",
    updated_at: "2025-01-01T01:00:00Z",
    ...overrides,
  };
}

function ev(overrides: Record<string, unknown> = {}) {
  return {
    sequence: 1,
    timestamp: "2025-01-01T00:00:01Z",
    event_type: "state_change",
    task_id: "task-0001",
    session_id: "sess-1",
    state: "running",
    runtime_state: "alive",
    ...overrides,
  };
}

describe("TaskMonitorPanel", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockListTasks.mockResolvedValue([]);
    mockListTaskEvents.mockResolvedValue([]);
    mockRequeueTask.mockResolvedValue({
      schema_version: 1,
      command: "requeue",
      task_id: "task-0001",
      state: "queued",
      runtime_state: "exited",
      version: 2,
      accepted: true,
      idempotent: false,
    });
  });
  afterEach(() => cleanup());

  it("renders the panel header", () => {
    render(<TaskMonitorPanel />);
    expect(screen.getByText("Tasks")).toBeInTheDocument();
  });

  it("shows empty state", async () => {
    render(<TaskMonitorPanel />);
    fireEvent.click(screen.getByLabelText("Expand tasks"));
    await waitFor(() => {
      expect(screen.getByText("No background tasks")).toBeInTheDocument();
    });
  });

  it("shows error when fetch fails", async () => {
    mockListTasks.mockRejectedValue(new Error("Network error"));
    render(<TaskMonitorPanel />);
    fireEvent.click(screen.getByLabelText("Expand tasks"));
    await waitFor(() => {
      expect(screen.getByText(/Network/)).toBeInTheDocument();
    });
  });

  it("renders tasks with state badges", async () => {
    mockListTasks.mockResolvedValue([
      snap({ task_id: "a1", state: "running" }),
      snap({ task_id: "b2", state: "failed" }),
    ]);
    render(<TaskMonitorPanel />);
    fireEvent.click(screen.getByLabelText("Expand tasks"));
    await waitFor(() => {
      expect(screen.getByText("a1")).toBeInTheDocument();
    });
    expect(screen.getByText("Running")).toBeInTheDocument();
    expect(screen.getByText("Failed")).toBeInTheDocument();
  });

  it("shows lifecycle state separately from runtime liveness", async () => {
    mockListTasks.mockResolvedValue([
      snap({ task_id: "failed-1", state: "failed", runtime_state: "exited" }),
      snap({ task_id: "legacy-1", state: "running", runtime_state: undefined }),
    ]);
    render(<TaskMonitorPanel />);
    fireEvent.click(screen.getByLabelText("Expand tasks"));
    await waitFor(() => {
      expect(screen.getByText("failed-1")).toBeInTheDocument();
    });
    expect(screen.getByText("Exited")).toBeInTheDocument();
    expect(screen.getByText("Runtime unknown")).toBeInTheDocument();
  });

  it("requeues a failed exited task without calling resume", async () => {
    mockListTasks.mockResolvedValue([
      snap({ task_id: "failed-1", state: "failed", runtime_state: "exited", version: 7 }),
    ]);
    render(<TaskMonitorPanel />);
    fireEvent.click(screen.getByLabelText("Expand tasks"));
    await waitFor(() => {
      expect(screen.getByText("failed-1")).toBeInTheDocument();
    });
    fireEvent.click(screen.getByLabelText("Task failed-1 — Failed"));
    fireEvent.click(screen.getByRole("button", { name: "Requeue" }));
    await waitFor(() => {
      expect(mockRequeueTask).toHaveBeenCalledWith(
        "failed-1",
        7,
        "desktop-requeue-failed-1-7",
      );
    });
  });

  it("expands and collapses detail", async () => {
    mockListTasks.mockResolvedValue([snap({ task_id: "task-0001", state: "succeeded" })]);
    render(<TaskMonitorPanel />);
    fireEvent.click(screen.getByLabelText("Expand tasks"));
    await waitFor(() => {
      expect(screen.getByText("task-000")).toBeInTheDocument();
    });
    expect(screen.queryByText("Task ID")).not.toBeInTheDocument();

    fireEvent.click(screen.getByLabelText("Task task-000 — Succeeded"));
    await waitFor(() => {
      expect(screen.getByText("Task ID")).toBeInTheDocument();
    });

    fireEvent.click(screen.getByLabelText("Task task-000 — Succeeded"));
    await waitFor(() => {
      expect(screen.queryByText("Task ID")).not.toBeInTheDocument();
    });
  });

  it("loads events on expand", async () => {
    mockListTasks.mockResolvedValue([snap({ task_id: "task-0001", state: "failed" })]);
    mockListTaskEvents.mockResolvedValue([
      ev({ sequence: 1, event_type: "error", error_code: "CRASH" }),
    ]);
    render(<TaskMonitorPanel />);
    fireEvent.click(screen.getByLabelText("Expand tasks"));
    await waitFor(() => {
      expect(screen.getByText("task-000")).toBeInTheDocument();
    });
    fireEvent.click(screen.getByLabelText("Task task-000 — Failed"));
    await waitFor(() => {
      expect(screen.getByText("Recent Events")).toBeInTheDocument();
    });
    expect(screen.getByText(/CRASH/)).toBeInTheDocument();
  });

  it("shows event error", async () => {
    mockListTasks.mockResolvedValue([snap({ task_id: "task-0001" })]);
    mockListTaskEvents.mockRejectedValue(new Error("Event failure"));
    render(<TaskMonitorPanel />);
    fireEvent.click(screen.getByLabelText("Expand tasks"));
    await waitFor(() => {
      expect(screen.getByText("task-000")).toBeInTheDocument();
    });
    fireEvent.click(screen.getByLabelText("Task task-000 — Running"));
    await waitFor(() => {
      expect(screen.getByText(/Event/)).toBeInTheDocument();
    });
  });

  it("calls onClose", () => {
    const onClose = vi.fn();
    render(<TaskMonitorPanel onClose={onClose} />);
    fireEvent.click(screen.getByLabelText("Close task panel"));
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it("refreshes on button click", async () => {
    mockListTasks
      .mockResolvedValueOnce([])
      .mockResolvedValueOnce([snap({ task_id: "ok" })]);
    render(<TaskMonitorPanel />);
    fireEvent.click(screen.getByLabelText("Expand tasks"));
    await waitFor(() => {
      expect(screen.getByText("No background tasks")).toBeInTheDocument();
    });
    fireEvent.click(screen.getByLabelText("Refresh tasks"));
    await waitFor(() => {
      expect(screen.getByText("ok")).toBeInTheDocument();
    });
  });

  it("shows task count badge", async () => {
    mockListTasks.mockResolvedValue([snap({ task_id: "a" }), snap({ task_id: "b" })]);
    render(<TaskMonitorPanel />);
    await waitFor(() => {
      expect(screen.getByText("2")).toBeInTheDocument();
    });
  });

  it("shows terminal icon only for terminal tasks", async () => {
    mockListTasks.mockResolvedValue([
      snap({ task_id: "t1", state: "succeeded" }),
      snap({ task_id: "t2", state: "running" }),
    ]);
    render(<TaskMonitorPanel />);
    fireEvent.click(screen.getByLabelText("Expand tasks"));
    await waitFor(() => {
      expect(screen.getByText("t1")).toBeInTheDocument();
    });
    expect(document.querySelectorAll(".taskmonitor__terminal").length).toBe(1);
  });
});
