export const TRANSCRIPT_AT_BOTTOM_THRESHOLD_PX = 4;

export function nativeTranscriptDistanceFromBottom(element: {
  scrollHeight: number;
  scrollTop: number;
  clientHeight: number;
}) {
  return element.scrollHeight - element.scrollTop - element.clientHeight;
}

export function nativeTranscriptBottomTop(element: { scrollHeight: number; clientHeight: number }) {
  return Math.max(0, element.scrollHeight - element.clientHeight);
}

export function hasTranscriptScrollableRange(
  element: { scrollHeight: number; clientHeight: number },
  threshold = TRANSCRIPT_AT_BOTTOM_THRESHOLD_PX,
) {
  return nativeTranscriptBottomTop(element) > threshold;
}
