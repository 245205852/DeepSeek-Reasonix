import type { ProjectNode, RemoteProjectView, RemoteSessionView } from "./types";

interface RemoteProjectBindings {
  AddRemoteProject(hostId: string, workspace: string): Promise<RemoteProjectView>;
  RemoveRemoteProject(hostId: string, workspace: string): Promise<void>;
  ListRemoteProjects(): Promise<RemoteProjectView[]>;
  RemoteProjectSessions(hostId: string, workspace: string): Promise<RemoteSessionView[]>;
}

export function createMockRemoteProjects(): {
  bindings: RemoteProjectBindings;
  appendToTree: (tree: ProjectNode[]) => ProjectNode[];
} {
  let projects: RemoteProjectView[] = [{ hostId: "demo", workspace: "~/app" }];
  const sessions: Record<string, RemoteSessionView[]> = {
    "demo\u0000~/app": [
      { name: "intro", title: "远程会话演示 / Remote demo session", turns: 18, current: true, lastActivityAt: Date.now() - 45 * 60 * 1000 },
    ],
  };
  const key = (hostId: string, workspace: string) => `${hostId}\u0000${workspace}`;

  const bindings: RemoteProjectBindings = {
    async AddRemoteProject(hostId, workspace) {
      const existing = projects.find((project) => project.hostId === hostId && project.workspace === workspace);
      if (existing) return existing;
      const project = { hostId, workspace, title: `${hostId}:${workspace}` };
      projects = [...projects, project];
      return project;
    },
    async RemoveRemoteProject(hostId, workspace) {
      projects = projects.filter((project) => project.hostId !== hostId || project.workspace !== workspace);
    },
    async ListRemoteProjects() {
      return projects.slice();
    },
    async RemoteProjectSessions(hostId, workspace) {
      return (sessions[key(hostId, workspace)] ?? []).map((row) => ({ ...row }));
    },
  };

  return {
    bindings,
    appendToTree(tree) {
      for (const project of projects) {
        if (tree.some((node) => node.key === `project_remote_${project.hostId}_${project.workspace}`)) continue;
        tree.push({
          key: `project_remote_${project.hostId}_${project.workspace}`,
          kind: "project",
          label: project.workspace.split("/").filter(Boolean).pop() || project.workspace,
          root: project.workspace,
          remote: { hostId: project.hostId, workspace: project.workspace },
        });
      }
      return tree;
    },
  };
}
