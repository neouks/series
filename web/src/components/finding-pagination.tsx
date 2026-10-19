"use client";

import { ChevronLeftIcon, ChevronRightIcon } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Pagination, PaginationContent, PaginationItem } from "@/components/ui/pagination";
import { Select, SelectContent, SelectGroup, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";

export function FindingPagination({
  page,
  pageSize,
  total,
  onPageChange,
  onPageSizeChange,
  pageSizeOptions = [10, 20, 50, 100],
}: {
  page: number;
  pageSize: number;
  total: number;
  onPageChange: (page: number) => void;
  onPageSizeChange: (size: number) => void;
  pageSizeOptions?: number[];
}) {
  const pages = Math.max(1, Math.ceil(total / pageSize));
  const current = Math.max(1, Math.min(page, pages));
  const from = total === 0 ? 0 : (current - 1) * pageSize + 1;
  return (
    <footer className="findings-pagination">
      <div className="flex flex-wrap items-center gap-3 text-muted-foreground">
        <span>每页条数</span>
        <Select value={String(pageSize)} onValueChange={(value) => onPageSizeChange(Number(value))}>
          <SelectTrigger aria-label="每页条数" className="w-20">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectGroup>
              {pageSizeOptions.map((size) => (
                <SelectItem key={size} value={String(size)}>
                  {size}
                </SelectItem>
              ))}
            </SelectGroup>
          </SelectContent>
        </Select>
        <span className="tabular-nums">
          {from}–{Math.min(current * pageSize, total)} / 共 {total} 条
        </span>
      </div>
      <Pagination className="mx-0 w-auto justify-start" aria-label="发现分页">
        <PaginationContent>
          <PaginationItem>
            <Button
              variant="ghost"
              disabled={current === 1}
              onClick={() => onPageChange(current - 1)}
              aria-label="上一页"
            >
              <ChevronLeftIcon data-icon="inline-start" />
              上一页
            </Button>
          </PaginationItem>
          <PaginationItem>
            <span className="findings-page-number" aria-current="page">
              {current}
            </span>
          </PaginationItem>
          <PaginationItem>
            <span className="text-muted-foreground">/ {pages}</span>
          </PaginationItem>
          <PaginationItem>
            <Button
              variant="ghost"
              disabled={current === pages}
              onClick={() => onPageChange(current + 1)}
              aria-label="下一页"
            >
              下一页
              <ChevronRightIcon data-icon="inline-end" />
            </Button>
          </PaginationItem>
        </PaginationContent>
      </Pagination>
    </footer>
  );
}
