import type { RemoteProjectView, RemoteSessionView } from "./remoteTypes";

export interface RemoteProjectBindings {
  AddRemoteProject(hostId: string, workspace: string): Promise<RemoteProjectView>;
  RemoveRemoteProject(hostId: string, workspace: string): Promise<void>;
  ListRemoteProjects(): Promise<RemoteProjectView[]>;
  RemoteProjectSessions(hostId: string, workspace: string): Promise<RemoteSessionView[]>;
}
