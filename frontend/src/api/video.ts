import { postForm, postJson } from './client'
import { normalizeVideoList } from './normalize'
import type { Video } from './types'

export function publishVideo(input: { title: string; description: string; play_url: string; cover_url: string }) {
  return postJson<Video>('/video/publish', input, { authRequired: true })
}

export type UploadResponse = { url: string; play_url?: string; cover_url?: string }
export type InitVideoUploadResponse = { upload_id: string; chunk_size: number; total_chunks: number }
export type ChunkUploadProgress = { uploadedChunks: number; totalChunks: number; percent: number }

export function uploadVideo(file: File) {
  const fd = new FormData()
  fd.append('file', file)
  return postForm<UploadResponse>('/video/uploadVideo', fd, { authRequired: true })
}

export async function uploadVideoInChunks(file: File, onProgress?: (progress: ChunkUploadProgress) => void) {
  const init = await postJson<InitVideoUploadResponse>(
    '/video/uploadVideo/init',
    { file_name: file.name, file_size: file.size },
    { authRequired: true },
  )

  for (let index = 0; index < init.total_chunks; index += 1) {
    const start = index * init.chunk_size
    const end = Math.min(start + init.chunk_size, file.size)
    const fd = new FormData()
    fd.append('upload_id', init.upload_id)
    fd.append('chunk_index', String(index))
    fd.append('file', file.slice(start, end), `${file.name}.part${index}`)
    await postForm('/video/uploadVideo/chunk', fd, { authRequired: true })
    const uploadedChunks = index + 1
    onProgress?.({ uploadedChunks, totalChunks: init.total_chunks, percent: Math.round((uploadedChunks / init.total_chunks) * 100) })
  }

  return postJson<UploadResponse>('/video/uploadVideo/complete', { upload_id: init.upload_id }, { authRequired: true })
}

export function uploadCover(file: File) {
  const fd = new FormData()
  fd.append('file', file)
  return postForm<UploadResponse>('/video/uploadCover', fd, { authRequired: true })
}

export async function listByAuthorId(authorId: number) {
  const videos = await postJson<Video[] | null>('/video/listByAuthorID', { author_id: authorId })
  return normalizeVideoList(videos)
}

export function getDetail(id: number) {
  return postJson<Video>('/video/getDetail', { id })
}
